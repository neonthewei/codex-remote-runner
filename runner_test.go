package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var fakeAuth = []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"fake-access","refresh_token":"fake-refresh","id_token":"fake-id"}}`)

type memorySecrets struct {
	values   map[string][]byte
	fail     bool
	denyRead bool
	corrupt  bool
}

func (m *memorySecrets) Get(u, v string) ([]byte, error) {
	if m.denyRead {
		return nil, errors.New("access denied")
	}
	if m.corrupt {
		return []byte("incorrect saved data"), nil
	}
	raw, ok := m.values[u+"/"+v]
	if !ok {
		return nil, errors.New("missing")
	}
	return raw, nil
}
func (m *memorySecrets) Put(u, v string, raw []byte) error {
	if m.fail {
		return errors.New("OS store unavailable")
	}
	m.values[u+"/"+v] = append([]byte{}, raw...)
	return nil
}

type fakeClient struct {
	boxes       map[string]Box
	calls       [][]string
	convs       []Conversation
	jobs        []map[string]string
	processes   []Process
	removed     int
	uploads     int
	failSuspend bool
	lostCreate  bool
	failStart   bool
	shells      []string
	prompts     []string
}

func (f *fakeClient) Prompt(out any, prompt string, args ...string) error {
	f.prompts = append(f.prompts, prompt)
	return f.Call(out, args...)
}

func (f *fakeClient) Call(out any, args ...string) error {
	f.calls = append(f.calls, append([]string{}, args...))
	var value any = map[string]any{}
	switch args[0] {
	case "get":
		value = f.boxes[args[1]]
	case "create":
		fields := map[string]string{}
		meta := map[string]string{}
		for n := 1; n < len(args); n++ {
			if strings.HasPrefix(args[n], "--") && n+1 < len(args) && !strings.HasPrefix(args[n+1], "--") {
				fields[args[n]] = args[n+1]
				if args[n] == "--meta" {
					k, v, _ := strings.Cut(args[n+1], "=")
					meta[k] = v
				}
				n++
			}
		}
		b := Box{ID: "new-box", Name: fields["--name"], Org: fields["--org"], State: "running", Metadata: meta}
		f.boxes[b.ID] = b
		value = b
		if f.lostCreate {
			return errors.New("create response lost")
		}
	case "ls":
		bs := []Box{}
		for _, b := range f.boxes {
			bs = append(bs, b)
		}
		value = bs
	case "resume", "start":
		b := f.boxes[args[1]]
		b.State = "running"
		f.boxes[b.ID] = b
	case "suspend":
		if f.failSuspend {
			return errors.New("suspend response lost")
		}
		b := f.boxes[args[1]]
		b.State = "suspended"
		f.boxes[b.ID] = b
	case "ps":
		value = f.processes
	case "jobs":
		value = map[string]any{"jobs": f.jobs}
	case "agent":
		if args[1] == "ls" {
			value = map[string]any{"conversations": f.convs}
		} else {
			if f.failStart {
				return errors.New("start response lost")
			}
			value = StartResult{ID: "conv-one", Job: "job-one", Turn: 2}
		}
	default:
		return errors.New("unexpected fake call")
	}
	if out != nil {
		raw, _ := json.Marshal(value)
		return json.Unmarshal(raw, out)
	}
	return nil
}
func (f *fakeClient) Shell(box, cmd string) (string, error) {
	f.shells = append(f.shells, cmd)
	if strings.HasPrefix(cmd, "rm -f") {
		f.removed++
	}
	return "", nil
}
func (f *fakeClient) Credentials(box string, raw []byte) ([]byte, error) {
	if raw != nil {
		f.uploads++
		return nil, nil
	}
	return fakeAuth, nil
}
func setup(t *testing.T) (*Runner, *fakeClient, *memorySecrets) {
	t.Helper()
	s, e := openState(filepath.Join(t.TempDir(), "state"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(s.Close)
	f := &fakeClient{boxes: map[string]Box{}, processes: []Process{}, convs: []Conversation{}, jobs: []map[string]string{}}
	m := &memorySecrets{values: map[string][]byte{}}
	return &Runner{State: s, Client: f, Secrets: m}, f, m
}
func seedTask(t *testing.T, r *Runner, f *fakeClient) {
	t.Helper()
	v := Task{Ticket: "TASK-1", User: "owner", Provider: "codex", Org: "org-test", Name: "probe", Box: "probe", Phase: "active", Conversation: "conv-one"}
	if e := r.State.Save(v); e != nil {
		t.Fatal(e)
	}
	if e := r.State.Acquire(v.User, v.Ticket); e != nil {
		t.Fatal(e)
	}
	f.boxes[v.Box] = Box{ID: v.Box, Name: v.Name, Org: v.Org, State: "running", Metadata: map[string]string{"ticket": v.Ticket, "controller": r.State.Namespace}}
}

func TestAuthErrorsDoNotContainValues(t *testing.T) {
	_, e := validateAuth([]byte(`{"password":"dont-print-this"}`))
	if e == nil || strings.Contains(e.Error(), "dont-print-this") {
		t.Fatal(e)
	}
}
func TestBusyProfilesAndDifferentUsers(t *testing.T) {
	r, _, _ := setup(t)
	if e := r.State.Acquire("a", "TASK-1"); e != nil {
		t.Fatal(e)
	}
	if r.State.Acquire("a", "TASK-2") == nil {
		t.Fatal("concurrent profile allowed")
	}
	if e := r.State.Acquire("b", "TASK-2"); e != nil {
		t.Fatal(e)
	}
}
func TestLeaseSurvivesRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, e := openState(root)
	if e != nil {
		t.Fatal(e)
	}
	s.Acquire("a", "TASK-1")
	s.Close()
	s, e = openState(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if s.Acquire("a", "TASK-2") == nil {
		t.Fatal("lease lost")
	}
}
func TestProcessLockRejectsSecondController(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	s, e := openState(root)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s2, e := openState(root)
	if e == nil {
		s2.Close()
		t.Fatal("second controller acquired lock")
	}
}
func TestCheckpointRefusesRunningWork(t *testing.T) {
	for _, kind := range []string{"agent", "job", "process"} {
		t.Run(kind, func(t *testing.T) {
			r, f, m := setup(t)
			seedTask(t, r, f)
			switch kind {
			case "agent":
				f.convs = []Conversation{{Status: "running"}}
			case "job":
				f.jobs = []map[string]string{{"status": "running"}}
			case "process":
				f.processes = []Process{{Command: "/usr/local/bin/codex exec"}}
			}
			if _, e := r.Checkpoint("TASK-1"); e == nil {
				t.Fatal("accepted live work")
			}
			if len(m.values) != 0 || f.removed != 0 {
				t.Fatal("handoff happened")
			}
		})
	}
}
func TestSecretWriteFailureKeepsGuestAndLease(t *testing.T) {
	r, f, m := setup(t)
	seedTask(t, r, f)
	m.fail = true
	if _, e := r.Checkpoint("TASK-1"); e == nil {
		t.Fatal("write failure ignored")
	}
	if f.removed != 0 {
		t.Fatal("guest auth removed")
	}
	if r.State.Acquire("owner", "OTHER") == nil {
		t.Fatal("lease released")
	}
}
func TestSuspendFailureCanFinishWithoutReadingDeletedAuth(t *testing.T) {
	r, f, m := setup(t)
	seedTask(t, r, f)
	f.failSuspend = true
	if _, e := r.Checkpoint("TASK-1"); e == nil {
		t.Fatal("suspend failure ignored")
	}
	v, _ := r.State.Version("owner")
	task, _ := r.State.Task("TASK-1")
	if task.Phase != "credentials-removed" || len(m.values) != 1 {
		t.Fatal(task)
	}
	f.failSuspend = false
	if _, e := r.Checkpoint("TASK-1"); e != nil {
		t.Fatal(e)
	}
	after, _ := r.State.Version("owner")
	if v != after || len(m.values) != 1 {
		t.Fatal("rewrote saved version")
	}
}
func TestStaleVersionRefusesOverwrite(t *testing.T) {
	r, f, m := setup(t)
	seedTask(t, r, f)
	r.State.DB.Exec("INSERT INTO profiles VALUES('owner','newer')")
	if _, e := r.Checkpoint("TASK-1"); e == nil {
		t.Fatal("stale overwrite accepted")
	}
	if len(m.values) != 0 || f.removed != 0 {
		t.Fatal("stale handoff")
	}
}
func TestResumeRestoresLatestAndOmitsNewConversationFlags(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	if _, e := r.Checkpoint("TASK-1"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Run("TASK-1", "continue", "/data/home", ""); e != nil {
		t.Fatal(e)
	}
	if f.uploads != 1 {
		t.Fatal("missing restore")
	}
	for _, c := range f.calls {
		if len(c) > 1 && c[0] == "agent" && c[1] == "prompt" {
			if !contains(c, "--resume") || contains(c, "--cwd") || contains(c, "--provider") {
				t.Fatal(c)
			}
		}
	}
}
func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
func genericConfig() Config {
	return Config{User: "owner", Org: "org-test", Provider: "claude", Base: "main", VCPUs: 2, Memory: 4096}
}
func TestLostCreateDoesNotCreateAgain(t *testing.T) {
	r, f, _ := setup(t)
	f.lostCreate = true
	if _, e := r.Create("TASK-2", genericConfig(), false); e == nil {
		t.Fatal("expected lost create")
	}
	if _, e := r.Create("TASK-2", genericConfig(), false); e == nil {
		t.Fatal("duplicate allowed")
	}
	count := 0
	for _, c := range f.calls {
		if c[0] == "create" {
			count++
		}
	}
	if count != 1 {
		t.Fatal(count)
	}
	if _, e := r.Reconcile("TASK-2"); e != nil {
		t.Fatal(e)
	}
}
func TestOwnershipMismatchPreventsDelivery(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	r.Checkpoint("TASK-1")
	b := f.boxes["probe"]
	b.Org = "other"
	f.boxes["probe"] = b
	if _, e := r.Activate("TASK-1"); e == nil {
		t.Fatal("ownership mismatch accepted")
	}
	if f.uploads != 0 {
		t.Fatal("delivered auth")
	}
}
func TestClaudeNeverExportsCredentials(t *testing.T) {
	r, f, m := setup(t)
	if _, e := r.Create("TASK-2", genericConfig(), false); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Activate("TASK-2"); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Checkpoint("TASK-2"); e != nil {
		t.Fatal(e)
	}
	if len(m.values) > 0 || f.uploads > 0 || f.removed > 0 {
		t.Fatal("Claude credentials exported")
	}
}
func TestUnknownTurnRequiresReconciliation(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	f.failStart = true
	if _, e := r.Run("TASK-1", "one", "/data/home", ""); e == nil {
		t.Fatal("expected failure")
	}
	f.failStart = false
	if _, e := r.Run("TASK-1", "two", "/data/home", ""); e == nil {
		t.Fatal("blind retry allowed")
	}
	f.convs = []Conversation{{ID: "conv-one", Provider: "codex", Status: "idle"}}
	if _, e := r.RecoverTurn("TASK-1", "conv-one"); e != nil {
		t.Fatal(e)
	}
}
func TestBootstrapDoesNotSelectRepoSecretsOrSnapshot(t *testing.T) {
	r, f, _ := setup(t)
	c := genericConfig()
	c.Provider = "codex"
	c.Secrets = []string{"GH_TOKEN"}
	c.Snapshot = "sensitive"
	v, e := r.Bootstrap(c)
	if e != nil {
		t.Fatal(e)
	}
	if v.Phase != "active" {
		t.Fatal(v)
	}
	for _, args := range f.calls {
		if args[0] == "create" {
			if contains(args, "GH_TOKEN") || contains(args, "--snapshot") {
				t.Fatal(args)
			}
		}
	}
}
func TestPreparePinsRepoAndBlocksUnknownState(t *testing.T) {
	r, _, _ := setup(t)
	r.Create("TASK-2", genericConfig(), false)
	v, e := r.Prepare("TASK-2", "owner/repo", "main")
	if e != nil {
		t.Fatal(e)
	}
	if v.Branch != "task-2-agent" {
		t.Fatal(v)
	}
	if _, e = r.Prepare("TASK-2", "other/repo", "main"); e == nil {
		t.Fatal("changed repo")
	}
	v.Phase = "preparing"
	r.State.Save(v)
	if _, e = r.Run("TASK-2", "unknown", "/data/home", ""); e == nil {
		t.Fatal("ran in uncertain checkout")
	}
	if _, e = r.RecoverPrepare("TASK-2"); e != nil {
		t.Fatal(e)
	}
}
func TestConfigRejectsUnknownFieldsAndMultipleDocuments(t *testing.T) {
	for _, s := range []string{"token: do-not-print", "user: a\n---\nuser: b", "secrets: [GH_TOKEN=value]", "vcpus: -1"} {
		t.Run(s, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.yaml")
			os.WriteFile(p, []byte(s), 0600)
			_, e := loadConfig(p, func(string) (string, bool) { return "", false })
			if e == nil || strings.Contains(e.Error(), "do-not-print") {
				t.Fatal(e)
			}
		})
	}
}
func TestConfigEnvironmentOverrideAndDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("user: example\norg: org-test\nrepo: owner/repo\nvcpus: 4\n"), 0600)
	env := map[string]string{"CODEX_REMOTE_USER": "different", "CODEX_REMOTE_SECRETS": "GH_TOKEN, TEST_KEY"}
	c, e := loadConfig(p, func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if e != nil {
		t.Fatal(e)
	}
	if c.User != "different" || c.VCPUs != 4 || c.Memory != 4096 || !reflect.DeepEqual(c.Secrets, []string{"GH_TOKEN", "TEST_KEY"}) {
		t.Fatal(c)
	}
}
func TestCommandReorderingKeepsTicketAfterFlags(t *testing.T) {
	v, e := reorder([]string{"TASK-1", "--prompt-file", "/tmp/task.txt"})
	if e != nil || !reflect.DeepEqual(v, []string{"--prompt-file", "/tmp/task.txt", "TASK-1"}) {
		t.Fatal(v, e)
	}
}

func TestCreateTaskFlagWithoutTracker(t *testing.T) {
	for _, id := range []string{"fix-login", "my_task_1", "GH-42"} {
		t.Run(id, func(t *testing.T) {
			t.Setenv("CODEX_REMOTE_CONFIG", "")
			t.Setenv("CODEX_REMOTE_CREDENTIAL_BACKEND", "native")
			t.Setenv("CODEX_REMOTE_CREDENTIAL_KEY_FILE", "")
			fakeInstabox(t, `case "$1" in
create)
  task= namespace=
  for arg do
    case "$arg" in
      ticket=*) task=${arg#ticket=} ;;
      controller=*) namespace=${arg#controller=} ;;
    esac
  done
  printf '{"id":"test-vm","orgId":"org-test","metadata":{"ticket":"%s","controller":"%s"}}' "$task" "$namespace"
  ;;
suspend) printf '{}' ;;
*) exit 1 ;;
esac
`)
			root := filepath.Join(t.TempDir(), "state")
			args := []string{"--state", root, "create", "--task", id, "--user", "owner", "--org", "org-test", "--provider", "claude"}
			v, e := execute(args)
			if e != nil || v.(Task).Ticket != id {
				t.Fatal("opaque task ID failed", e)
			}
			s, e := openState(root)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			stored, e := s.Task(id)
			if e != nil || stored.Phase != "paused" || stored.Ticket != id {
				t.Fatal("task ID was not preserved", e)
			}
		})
	}
}

func TestSkillWrapperResolvesSymlinkedDirectory(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	skill := filepath.Join(checkout, "skills", "codex-remote")
	if e := os.MkdirAll(filepath.Join(skill, "scripts"), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll(filepath.Join(checkout, "bin"), 0700); e != nil {
		t.Fatal(e)
	}
	raw, e := os.ReadFile("skills/codex-remote/scripts/run.sh")
	if e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(skill, "scripts", "run.sh"), raw, 0600)
	os.WriteFile(filepath.Join(checkout, "bin", "codex-remote"), []byte("#!/bin/sh\nprintf 'wrapper-ok:%s' \"$1\"\n"), 0700)
	link := filepath.Join(root, "agent-skill")
	if e = os.Symlink(skill, link); e != nil {
		t.Fatal(e)
	}
	out, e := exec.Command("sh", filepath.Join(link, "scripts", "run.sh"), "--help").CombinedOutput()
	if e != nil || string(out) != "wrapper-ok:--help" {
		t.Fatalf("wrapper: %q %v", out, e)
	}
}

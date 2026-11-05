package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthRejectsMixedAccountAndAPIKey(t *testing.T) {
	raw := bytes.Replace(fakeAuth, []byte(`"auth_mode":"chatgpt"`), []byte(`"auth_mode":"chatgpt","OPENAI_API_KEY":"fake-key"`), 1)
	if _, e := validateAuth(raw); e == nil {
		t.Fatal("mixed auth accepted")
	}
}

func TestVersionDoesNotOpenConfigurationOrState(t *testing.T) {
	t.Setenv("CODEX_REMOTE_CONFIG", "/nonexistent/config.yaml")
	t.Setenv("CODEX_REMOTE_STATE", "/nonexistent/state")
	v, e := execute([]string{"--version"})
	if e != nil || v.(map[string]string)["version"] != Version {
		t.Fatal(v, e)
	}
}

func TestRunRejectsAlternateCredentialRoutes(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	if _, e := r.Run("TASK-1", "test", "/data/home", ""); e != nil {
		t.Fatal(e)
	}
	check := f.shells[0]
	for _, override := range []string{"", "CODEX_HOME=/tmp/other", "CLAUDE_CONFIG_DIR=/tmp/other", "OPENAI_API_KEY=fake", "ANTHROPIC_AUTH_TOKEN=fake"} {
		t.Run(override, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", check)
			cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
			if override != "" {
				cmd.Env = append(cmd.Env, override)
			}
			e := cmd.Run()
			if (override == "") != (e == nil) {
				t.Fatal("incorrect auth-route decision")
			}
		})
	}
}

func TestCheckpointRequiresReadableIdenticalBackup(t *testing.T) {
	for _, fault := range []string{"denied", "corrupt"} {
		t.Run(fault, func(t *testing.T) {
			r, f, m := setup(t)
			seedTask(t, r, f)
			m.denyRead = fault == "denied"
			m.corrupt = fault == "corrupt"
			if _, e := r.Checkpoint("TASK-1"); e == nil {
				t.Fatal("unusable backup accepted")
			}
			v, e := r.State.Version("owner")
			if e != nil || v != "" || f.removed != 0 {
				t.Fatal("guest or current version changed")
			}
			if r.State.Acquire("owner", "OTHER") == nil {
				t.Fatal("failed backup released lease")
			}
		})
	}
}

func TestCheckpointRejectsIncompleteOrUnknownWorkState(t *testing.T) {
	for _, fault := range []string{"conversations", "jobs", "processes", "agent-state", "job-state", "process-command", "claude-node"} {
		t.Run(fault, func(t *testing.T) {
			r, f, m := setup(t)
			seedTask(t, r, f)
			switch fault {
			case "conversations":
				f.convs = nil
			case "jobs":
				f.jobs = nil
			case "processes":
				f.processes = nil
			case "agent-state":
				f.convs = []Conversation{{Status: "new-provider-state"}}
			case "job-state":
				f.jobs = []map[string]string{{"status": "new-provider-state"}}
			case "process-command":
				f.processes = []Process{{}}
			case "claude-node":
				f.processes = []Process{{Command: "node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}}
			}
			if _, e := r.Checkpoint("TASK-1"); e == nil {
				t.Fatal("ambiguous or running work accepted")
			}
			if f.removed != 0 || len(m.values) != 0 {
				t.Fatal("handoff happened with ambiguous work")
			}
		})
	}
}

func TestCheckpointResumesBeforeReadingCachedProcessState(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	b := f.boxes["probe"]
	b.State = "suspended"
	f.boxes[b.ID] = b
	if _, e := r.Checkpoint("TASK-1"); e != nil {
		t.Fatal(e)
	}
	resume, process := -1, -1
	for n, c := range f.calls {
		if c[0] == "resume" {
			resume = n
		}
		if c[0] == "ps" {
			process = n
		}
	}
	if resume < 0 || process <= resume {
		t.Fatal("used cached paused process data")
	}
}

func TestReconcileAfterAutoSuspend(t *testing.T) {
	r, f, _ := setup(t)
	f.lostCreate = true
	if _, e := r.Create("TASK-2", genericConfig(), false); e == nil {
		t.Fatal("expected lost response")
	}
	b := f.boxes["new-box"]
	b.State = "suspended"
	f.boxes[b.ID] = b
	v, e := r.Reconcile("TASK-2")
	if e != nil || v.Box != b.ID || v.Phase != "paused" {
		t.Fatal(v, e)
	}
}

func TestRunKeepsTaskTextOutOfArguments(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	prompt := "private task description"
	if _, e := r.Run("TASK-1", prompt, "/data/home", ""); e != nil {
		t.Fatal(e)
	}
	if len(f.prompts) != 1 || f.prompts[0] != prompt {
		t.Fatal("task text not delivered")
	}
	for _, call := range f.calls {
		if strings.Contains(strings.Join(call, " "), prompt) {
			t.Fatal("task text leaked into arguments")
		}
	}
}

func fakeInstabox(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "instabox")
	if e := os.WriteFile(p, []byte("#!/bin/sh\nset -eu\n"+script), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestNativePromptTransportAndShellResponse(t *testing.T) {
	dir := fakeInstabox(t, "printf '%s\\n' \"$@\" > \"$AUDIT_ARGS\"\ncat > \"$AUDIT_STDIN\"\nprintf '%s' '{\"conversationId\":\"test\"}'\n")
	argsFile, inputFile := filepath.Join(dir, "args"), filepath.Join(dir, "stdin")
	t.Setenv("AUDIT_ARGS", argsFile)
	t.Setenv("AUDIT_STDIN", inputFile)
	var v StartResult
	if e := (Instabox{}).Prompt(&v, "private task", "agent", "prompt", "box"); e != nil {
		t.Fatal(e)
	}
	args, _ := os.ReadFile(argsFile)
	input, _ := os.ReadFile(inputFile)
	if v.ID != "test" || strings.Contains(string(args), "private task") || string(input) != "private task" || !strings.Contains(string(args), "-\n--json\n") {
		t.Fatal("incorrect stdin transport")
	}
	for _, response := range []string{`{}`, `null`, `{"exitCode":1}`, `{"exitCode":0,"stdout":"ok"}`} {
		t.Run(response, func(t *testing.T) {
			fakeInstabox(t, "printf '%s' "+quote(response)+"\n")
			out, e := (Instabox{}).Shell("box", "true")
			if response == `{"exitCode":0,"stdout":"ok"}` {
				if e != nil || out != "ok" {
					t.Fatal(out, e)
				}
			} else if e == nil {
				t.Fatal("incomplete or failed exec response accepted")
			}
		})
	}
}

// Opt in on a desktop or isolated Linux session with an unlocked native store.
// Only this test's synthetic entry is created and removed. No real auth is read.
func TestNativeCredentialStoreRoundTrip(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_TEST_OS_STORE") != "1" {
		t.Skip("set CODEX_REMOTE_TEST_OS_STORE=1 to test the native credential store")
	}
	s := &OSSecrets{service: "codex-remote-audit/" + randomID()}
	version := randomID()
	if e := s.Put("audit", version, fakeAuth); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.ring.Remove(s.key("audit", version)); e != nil {
			t.Error("cannot remove synthetic audit entry")
		}
	})
	got, e := s.Get("audit", version)
	if e != nil || !bytes.Equal(got, fakeAuth) {
		t.Fatal("native store round-trip failed (no credential data logged)")
	}
	other := &OSSecrets{service: "codex-remote-audit/" + randomID()}
	if _, e := other.Get("audit", version); e == nil {
		t.Fatal("separate controller namespaces share a credential item")
	}
}

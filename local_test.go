package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fakeLocalControl struct {
	online   bool
	prepared bool
	served   bool
	result   StartResult
	thread   map[string]any
	pairing  string
}

func (f *fakeLocalControl) Prepare(Task, string, string) error {
	f.prepared = true
	return nil
}

func (f *fakeLocalControl) Run(Task, string, string) (StartResult, error) {
	if f.result.ID == "" {
		return StartResult{}, errors.New("missing result")
	}
	return f.result, nil
}

func (f *fakeLocalControl) Thread(Task) (map[string]any, error) {
	return f.thread, nil
}

func (f *fakeLocalControl) Pair(Task, string) (string, error) {
	if f.pairing == "" {
		return "", errors.New("missing pairing")
	}
	return f.pairing, nil
}

func (f *fakeLocalControl) Online(Task) bool {
	return f.online
}

func (f *fakeLocalControl) Serve(Task) error {
	f.served = true
	return nil
}

func localConfig(root string) Config {
	c := defaultConfig()
	c.Host = "local"
	c.Backend = "t3"
	c.User = "owner"
	c.Repo = "owner/repo"
	c.LocalRepoRoot = filepath.Join(root, "repo")
	c.LocalWorktreeRoot = filepath.Join(root, "worktrees")
	return c
}

func TestLocalLifecycleAndSingleWriterHandoff(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{result: StartResult{ID: "t3-thread", Job: "run-one"}, thread: map[string]any{"status": "completed"}}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-LOCAL", c, false)
	if err != nil || task.Host != "local" || task.Owner != "t3" {
		t.Fatal(task, err)
	}
	task, err = r.Prepare(task.Ticket, c.Repo, c.Base)
	if err != nil || !control.prepared || task.Phase != "active" {
		t.Fatal(task, err)
	}
	if _, err = r.Run(task.Ticket, "probe", "", "gpt-test"); err == nil {
		t.Fatal("offline T3 accepted work")
	}
	control.online = true
	result, err := r.Run(task.Ticket, "probe", "", "gpt-test")
	if err != nil || result.ID != "t3-thread" {
		t.Fatal(result, err)
	}
	if _, err = r.HandoffLocal(task.Ticket, "app"); err == nil {
		t.Fatal("handoff ignored live T3 writer")
	}
	control.online = false
	task, err = r.HandoffLocal(task.Ticket, "app")
	if err != nil || task.Owner != "app" {
		t.Fatal(task, err)
	}
	if _, err = r.Run(task.Ticket, "blocked", "", ""); err == nil {
		t.Fatal("T3 wrote while Codex app owned the task")
	}
	control.online = true
	task, err = r.HandoffLocal(task.Ticket, "t3")
	if err != nil || task.Owner != "t3" {
		t.Fatal(task, err)
	}
	status, err := r.Status(task.Ticket)
	if err != nil || status["t3_online"] != true || status["thread"] == nil {
		t.Fatal(status, err)
	}
}

func TestLocalConfigurationRequiresBoundedPaths(t *testing.T) {
	c := localConfig(t.TempDir())
	c.LocalWorktreeRoot = "relative"
	if err := c.Validate(); err == nil {
		t.Fatal("relative worktree root accepted")
	}
	c = localConfig(t.TempDir())
	c.LocalT3Port = 80
	if err := c.Validate(); err == nil {
		t.Fatal("privileged local port accepted")
	}
}

func TestLocalUncertainTurnRecoversOnlyVerifiedCodexThread(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true, thread: map[string]any{"thread": map[string]any{"threadId": "mcp:verified", "providerInstanceId": "codex", "latestRunId": "run:verified"}}}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-RECOVER", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "starting-turn"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.State.DB.Exec("INSERT INTO turns(ticket,conversation,job,mode,status) VALUES(?,?,?,'local','uncertain')", task.Ticket, "", ""); err != nil {
		t.Fatal(err)
	}
	task, err = r.RecoverTurn(task.Ticket, "mcp:verified")
	if err != nil || task.Phase != "active" || task.Conversation != "mcp:verified" {
		t.Fatal(task, err)
	}
	var job, status string
	if err = r.State.DB.QueryRow("SELECT job,status FROM turns WHERE ticket=?", task.Ticket).Scan(&job, &status); err != nil || job != "run:verified" || status != "active" {
		t.Fatal(job, status, err)
	}
}

func TestLocalPrepareDoesNotHideUncertainState(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-PREPARE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Repo = c.Repo
	task.Base = c.Base
	task.Branch = "task-prepare-agent"
	task.Phase = "preparing"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Prepare(task.Ticket, c.Repo, c.Base); err == nil {
		t.Fatal("uncertain local preparation was reported as complete")
	}
}

func TestLocalPairingStaysInSelectedFile(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true, pairing: `{"url":"https://private.example/pair#token=secret"}`}
	r.Local = control
	c := localConfig(t.TempDir())
	c.T3PublicURL = "https://private.example"
	r.Config = c
	task, err := r.Create("TASK-PAIR", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "active"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "pairing.json")
	if _, err = t3Pair(r, task.Ticket, path); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != control.pairing {
		t.Fatal(string(raw), err)
	}
}

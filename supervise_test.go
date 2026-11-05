package main

import (
	"encoding/json"
	"errors"
	"github.com/gofrs/flock"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSupervisorWaitsForExactTurnAndCheckpointsOnCompletion(t *testing.T) {
	o, c := testObserver(t)
	n := 0
	watcher := supervisor{state: o.state, client: &linearEventClient{observationClient: c, job: "new-job", status: "running"}, checkpoint: func(id string) error { n++; return nil }}
	turn := supervisedTurn{Task: "TASK-1", Conversation: "conv-one", Job: "new-job", Mode: "host", Status: "active"}
	if done, e := watcher.inspect(turn); done || e != nil || n != 0 {
		t.Fatal(done, e, n)
	}
	watcher.client = &linearEventClient{observationClient: c, job: "old-job", status: "idle"}
	if done, e := watcher.inspect(turn); done || !errors.Is(e, errReconcileRetry) || n != 0 {
		t.Fatal("stale job completed task", done, e, n)
	}
	watcher.client = &linearEventClient{observationClient: c, job: "new-job", status: "idle"}
	if done, e := watcher.inspect(turn); !done || e != nil || n != 1 {
		t.Fatal(done, e, n)
	}
}
func TestSupervisorPreservesUnknownOrPausedWork(t *testing.T) {
	o, c := testObserver(t)
	n := 0
	watcher := supervisor{state: o.state, client: &linearEventClient{observationClient: c, job: "new-job", status: "unknown"}, checkpoint: func(id string) error { n++; return nil }}
	turn := supervisedTurn{Task: "TASK-1", Conversation: "conv-one", Job: "new-job"}
	if done, e := watcher.inspect(turn); done || e == nil || n != 0 {
		t.Fatal(done, e, n)
	}
	b := c.boxes["probe"]
	b.State = "suspended"
	c.boxes["probe"] = b
	if done, e := watcher.inspect(turn); done || e == nil || n != 0 {
		t.Fatal(done, e, n)
	}
	if len(c.shells) != 0 {
		t.Fatal("woke paused VM")
	}
}
func TestSupervisorCheckpointFailureDoesNotReportSuccess(t *testing.T) {
	o, c := testObserver(t)
	watcher := supervisor{state: o.state, client: &linearEventClient{observationClient: c, job: "job", status: "idle"}, checkpoint: func(id string) error { return errors.New("credential store unavailable") }}
	if done, e := watcher.inspect(supervisedTurn{Task: "TASK-1", Conversation: "conv-one", Job: "job"}); done || e == nil {
		t.Fatal(done, e)
	}
	watcher.checkpoint = func(id string) error { return errors.New("controller busy") }
	if done, e := watcher.inspect(supervisedTurn{Task: "TASK-1", Conversation: "conv-one", Job: "job"}); done || !errors.Is(e, errReconcileRetry) {
		t.Fatal("transient contention must retry", done, e)
	}
}
func TestCheckpointSavesFinalObservationForPausedDashboard(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	client := &observationClient{fakeClient: f}
	r.Client = client
	// Checkpoint has authoritative idle process/job checks before the capture.
	f.convs = []Conversation{{ID: "conv-one", Provider: "codex", Status: "idle"}}
	task, e := r.Checkpoint("TASK-1")
	if e != nil {
		t.Fatal(e)
	}
	if task.Phase != "paused" {
		t.Fatal(task)
	}
	saved, e := readSavedObservation(r.State.Root, task)
	if e != nil || len(saved.Events) != 150 {
		t.Fatal(e, len(saved.Events))
	}
	ro, e := openObservationState(r.State.Root)
	if e != nil {
		t.Fatal(e)
	}
	defer ro.DB.Close()
	o := &observer{state: ro, client: client}
	count := len(f.calls)
	v := o.collect(task)
	if len(v.Events) != 150 || len(f.calls) != count+1 || v.VM != "suspended" {
		t.Fatal("paused view lost saved evidence or read guest", len(v.Events), v.VM, f.calls[count:])
	}
	raw, _ := json.Marshal(v)
	if string(raw) == "" {
		t.Fatal("invalid view")
	}
}

func TestObservationStorageFailurePreservesAuthAndLease(t *testing.T) {
	r, f, _ := setup(t)
	seedTask(t, r, f)
	f.convs = []Conversation{{ID: "conv-one", Provider: "codex", Status: "idle"}}
	// An insecure directory must not become a destination for private evidence.
	path := filepath.Join(r.State.Root, "observations")
	if e := os.Mkdir(path, 0755); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Checkpoint("TASK-1"); e == nil {
		t.Fatal("insecure evidence storage accepted")
	}
	if f.removed != 0 {
		t.Fatal("guest auth removed after evidence save failure")
	}
	var owner string
	if e := r.State.DB.QueryRow("SELECT ticket FROM leases WHERE user='owner'").Scan(&owner); e != nil || owner != "TASK-1" {
		t.Fatal("lease lost", e, owner)
	}
}

func TestCLIRunRefusesWithoutSupervisorBeforeStartingVM(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	state, e := openState(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = state.Save(Task{Ticket: "TASK-1", User: "owner", Provider: "codex", Org: "org-test", Name: "task-one", Box: "box-one", Phase: "active", Host: "instabox"}); e != nil {
		t.Fatal(e)
	}
	state.Close()
	_, e = execute([]string{"--state", root, "run", "TASK-1", "--prompt-file", "/nonexistent/task"})
	if e == nil || !strings.Contains(e.Error(), "persistent host service") {
		t.Fatal(e)
	}
	lock := flock.New(filepath.Join(root, "supervisor.lock"))
	ok, e := lock.TryLock()
	if e != nil || !ok {
		t.Fatal(e)
	}
	defer lock.Unlock()
	_, e = execute([]string{"--state", root, "run", "TASK-1", "--prompt-file", "/nonexistent/task"})
	if e == nil || !strings.Contains(e.Error(), "cannot read task-text") {
		t.Fatal("live supervisor not recognized", e)
	}
}

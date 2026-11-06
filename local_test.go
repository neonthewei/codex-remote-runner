package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type fakeLocalControl struct {
	healthStatus  string
	healthErr     error
	healthCalls   int
	online        bool
	prepared      bool
	served        bool
	stopped       bool
	branch        string
	branchMissing bool
	prepareErr    error
	result        StartResult
	thread        map[string]any
	pairing       string
	managed       string
	managedRoot   string
	projectID     string
	runCalls      int
}

func (f *fakeLocalControl) Health() (map[string]string, error) {
	f.healthCalls++
	if f.healthErr != nil {
		return nil, f.healthErr
	}
	status := f.healthStatus
	if status == "" {
		status = "ready"
	}
	return map[string]string{"status": status}, nil
}

func (f *fakeLocalControl) Archive(_ Task, archived bool) (any, error) {
	if archived {
		f.managed = "archived"
	} else {
		f.managed = "active"
	}
	return map[string]string{"status": f.managed}, nil
}

func (f *fakeLocalControl) Pin(_ Task, pinned bool) (any, error) {
	if pinned {
		f.managed = "pinned"
	} else {
		f.managed = "unpinned"
	}
	return map[string]string{"status": f.managed}, nil
}

func (f *fakeLocalControl) Project(_ Task, projectID string) (any, error) {
	f.managed = "project-updated"
	f.projectID = projectID
	return map[string]string{"status": f.managed, "project_id": projectID}, nil
}

func (f *fakeLocalControl) Rename(_ Task, title string) (any, error) {
	f.managed = "renamed"
	f.projectID = title
	return map[string]string{"status": f.managed, "title": title}, nil
}

func (f *fakeLocalControl) Projects(Task) (any, error) {
	return map[string]any{"projects": []any{}}, nil
}

func (f *fakeLocalControl) Adopt(_ Task, threadID string) (any, error) {
	f.projectID = threadID
	return map[string]string{"status": "adopted", "thread_id": threadID}, nil
}

func (f *fakeLocalControl) CreateAppThread(Task, string, string) (string, error) {
	return "created-native-thread", nil
}

func (f *fakeLocalControl) AppRun(task Task, _ string, _ bool) (StartResult, error) {
	return StartResult{ID: task.AppThread, Job: "app-turn"}, nil
}

func (f *fakeLocalControl) Prepare(Task, string, string) error {
	f.prepared = true
	return f.prepareErr
}

func (f *fakeLocalControl) PrepareManagedApp(task Task, _ string, branch string, _ bool) (managedAppTask, error) {
	f.prepared = true
	worktree := task.Worktree
	if f.managedRoot != "" {
		worktree = f.managedRoot
	}
	threadID := task.AppThread
	if threadID == "" {
		threadID = "managed-app-thread"
	}
	return managedAppTask{ThreadID: threadID, Worktree: worktree, Branch: branch, ProjectID: "saved-project", ProjectName: "SignalsurfWeb"}, f.prepareErr
}

func (f *fakeLocalControl) Run(Task, string, string) (StartResult, error) {
	f.runCalls++
	if f.result.ID == "" {
		return StartResult{}, errors.New("missing result")
	}
	return f.result, nil
}

func TestLocalFirstRunRequiresModelBeforeUncertainState(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-MODEL-PREFLIGHT", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "active"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Run(task.Ticket, "probe", "", ""); err == nil || err.Error() != "first local T3 run requires --model" {
		t.Fatal(err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Phase != "active" || control.runCalls != 0 {
		t.Fatal(stored, control.runCalls, err)
	}
	var turns int
	if err = r.State.DB.QueryRow("SELECT count(*) FROM turns WHERE ticket=?", task.Ticket).Scan(&turns); err != nil || turns != 0 {
		t.Fatal(turns, err)
	}
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

func (f *fakeLocalControl) Stop(Task) error {
	f.stopped = true
	f.online = false
	return nil
}

func (f *fakeLocalControl) PreparePreview(Task) error {
	f.managed = "preview-prepared"
	return nil
}

func (f *fakeLocalControl) ServeWeb(Task, string) error {
	f.managed = "web-served"
	return nil
}

func (f *fakeLocalControl) WebStatus(Task, string) (map[string]any, error) {
	return map[string]any{"status": "online", "url": "https://example.trycloudflare.com"}, nil
}

func (f *fakeLocalControl) StopWeb(Task) error {
	f.managed = "web-stopped"
	return nil
}

func (f *fakeLocalControl) WebLogs(Task, int) (map[string]any, error) {
	return map[string]any{"lines": []string{"ready"}}, nil
}

func (f *fakeLocalControl) WebInspect(Task, string) (webInspection, error) {
	return webInspection{URL: "https://example.trycloudflare.com", Screenshot: []byte("png")}, nil
}

func (f *fakeLocalControl) Branch(t Task) (string, error) {
	if f.branchMissing && !f.prepared {
		return "", errors.New("missing worktree")
	}
	if f.branch != "" {
		return f.branch, nil
	}
	if f.prepared {
		return t.Branch, nil
	}
	return "task-branch", nil
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

func TestWorktreeTaskCodexAppManagementUsesRecordedTask(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-APP-MANAGEMENT", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Conversation = "mcp:thread"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"archive-task", "unarchive-task", "pin-task", "unpin-task", "list-projects"} {
		if _, err = r.ManageCodexAppTask(task.Ticket, action, ""); err != nil {
			t.Fatal(action, err)
		}
	}
	if _, err = r.ManageCodexAppTask(task.Ticket, "project-task", "project-one"); err != nil || control.projectID != "project-one" {
		t.Fatal(control.projectID, err)
	}
	if _, err = r.ManageCodexAppTask(task.Ticket, "rename-task", "Renamed task"); err != nil || control.projectID != "Renamed task" {
		t.Fatal(control.projectID, err)
	}
}

func TestWorktreeTaskAdoptsAndRunsNativeCodexAppThread(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-APP-ADOPT", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "active"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	task, err = r.AdoptCodexAppThread(task.Ticket, "native-thread")
	if err != nil || task.AppThread != "native-thread" || task.Owner != "app" {
		t.Fatal(task, err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.AppThread != "native-thread" {
		t.Fatal(stored, err)
	}
	result, err := r.RunCodexAppThread(task.Ticket, "continue", true)
	if err != nil || result.ID != "native-thread" || result.Job != "app-turn" {
		t.Fatal(result, err)
	}
}

func TestWorktreeTaskRefusesAdoptionWhileT3IsOnline(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-APP-ADOPT-BUSY", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "active"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AdoptCodexAppThread(task.Ticket, "native-thread"); err == nil {
		t.Fatal("online T3 writer allowed native thread adoption")
	}
}

func TestWorktreeTaskCreatesNativeCodexAppThread(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-APP-CREATE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Phase = "active"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	task, err = r.CreateCodexAppThread(task.Ticket, "Created thread", "project-one")
	if err != nil || task.AppThread != "created-native-thread" || task.Owner != "app" {
		t.Fatal(task, err)
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

func TestLocalRecoverPrepareResumesMissingWorktree(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{branchMissing: true}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-RECOVER-PREPARE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Repo = c.Repo
	task.Base = c.Base
	task.Branch = "task-recover-prepare-agent"
	task.Phase = "preparing"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	task, err = r.RecoverPrepare(task.Ticket)
	if err != nil || !control.prepared || task.Phase != "active" {
		t.Fatal(task, err)
	}
}

func TestLocalRecoverPrepareKeepsPreparingWhenSetupFails(t *testing.T) {
	r, _, _ := setup(t)
	control := &fakeLocalControl{branch: "task-setup-failure-agent", prepareErr: errors.New("setup failed")}
	r.Local = control
	c := localConfig(t.TempDir())
	r.Config = c
	task, err := r.Create("TASK-SETUP-FAILURE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Repo = c.Repo
	task.Base = c.Base
	task.Branch = "task-setup-failure-agent"
	task.Phase = "preparing"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RecoverPrepare(task.Ticket); err == nil {
		t.Fatal("failed setup was reported as recovered")
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Phase != "preparing" {
		t.Fatal(stored, err)
	}
}

func TestLocalHostPrepareRunsPinnedSetupForExistingBranch(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "worktrees", "task")
	if err := os.MkdirAll(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"checkout", "-b", "task-setup-agent"}} {
		cmd := exec.Command("git", append([]string{"-C", worktree}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	t.Setenv("CODEX_REMOTE_SETUP_HELPER", "1")
	host := &localHost{
		config:    Config{LocalWorktreeRoot: filepath.Join(root, "worktrees")},
		setupBin:  os.Args[0],
		setupArgs: []string{"-test.run=^TestLocalSetupProcess$"},
	}
	task := Task{Worktree: worktree}
	if err := host.Prepare(task, "main", "task-setup-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "setup.marker")); err != nil {
		t.Fatal(err)
	}
}

func TestLocalHostPrepareRunsPinnedSetupAfterCreatingWorktree(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote.git")
	worktree := filepath.Join(root, "worktrees", "task")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, command := range []struct {
		directory string
		args      []string
	}{
		{root, []string{"init", "--bare", remote}},
		{source, []string{"init", "-b", "main"}},
		{source, []string{"config", "user.email", "test@example.com"}},
		{source, []string{"config", "user.name", "Test"}},
	} {
		cmd := exec.Command("git", append([]string{"-C", command.directory}, command.args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	if err := os.WriteFile(filepath.Join(source, "tracked.txt"), []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"commit", "-m", "initial"}, {"remote", "add", "origin", remote}, {"push", "-u", "origin", "main"}} {
		cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(output), err)
		}
	}
	t.Setenv("CODEX_REMOTE_SETUP_HELPER", "1")
	host := &localHost{
		config:    Config{LocalWorktreeRoot: filepath.Join(root, "worktrees")},
		setupBin:  os.Args[0],
		setupArgs: []string{"-test.run=^TestLocalSetupProcess$"},
	}
	task := Task{SourceRoot: source, Worktree: worktree}
	if err := host.Prepare(task, "main", "task-setup-agent"); err != nil {
		t.Fatal(err)
	}
	if branch, err := host.Branch(task); err != nil || branch != "task-setup-agent" {
		t.Fatal(branch, err)
	}
	if _, err := os.Stat(filepath.Join(worktree, "setup.marker")); err != nil {
		t.Fatal(err)
	}
}

func TestLocalSetupProcess(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_SETUP_HELPER") != "1" {
		return
	}
	if err := os.WriteFile("setup.marker", []byte("ready"), 0600); err != nil {
		os.Exit(1)
	}
}

func TestRunLocalCommandWithTimeoutStopsHungProcess(t *testing.T) {
	t.Setenv("CODEX_REMOTE_TIMEOUT_HELPER", "1")
	started := time.Now()
	err := runLocalCommandWithTimeout(100*time.Millisecond, nil, os.Args[0], "-test.run=^TestLocalCommandTimeoutProcess$")
	if !errors.Is(err, errLocalCommandTimedOut) {
		t.Fatal(err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("hung local command did not stop promptly")
	}
}

func TestLocalCommandTimeoutProcess(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_TIMEOUT_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
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

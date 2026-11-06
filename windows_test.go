package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func windowsConfig() Config {
	c := defaultConfig()
	c.Host = "windows"
	c.Backend = "t3"
	c.Provider = "codex"
	c.User = "owner"
	c.Repo = "owner/repo"
	c.WindowsSSHTarget = "signal-windows"
	c.WindowsSSHBin = "ssh"
	c.WindowsRepoRoot = `C:/Users/NEON/Documents/Project`
	c.WindowsWorktreeRoot = `C:/Users/NEON/.codex/worktrees/remote-project`
	c.WindowsAppWorktreeRoot = `C:/Users/NEON/.codex/worktrees`
	c.WindowsStateRoot = `C:/Users/NEON/.codex/state/codex-remote-runner`
	c.WindowsT3Bin = `C:/Users/NEON/.t3/runtime/versions/0.1.7/t3.exe`
	c.WindowsCodexBin = `C:/Users/NEON/AppData/Local/OpenAI/Codex/bin/version/codex.exe`
	c.WindowsT3Port = 3784
	return c
}

func testWindowsWorkerPolicy() windowsWorkerPolicy {
	return windowsWorkerPolicy{
		RepoRoot:        `C:/Users/NEON/Documents/Project`,
		WorktreeRoot:    `C:/Users/NEON/.codex/worktrees/remote-project`,
		AppWorktreeRoot: `C:/Users/NEON/.codex/worktrees`,
		StateRoot:       `C:/Users/NEON/.codex/state/codex-remote-runner`,
		T3Bin:           `C:/Users/NEON/.t3/runtime/versions/0.1.7/t3.exe`,
		CodexBin:        `C:/Users/NEON/AppData/Local/OpenAI/Codex/bin/version/codex.exe`,
		T3Port:          3784,
	}
}

func TestWindowsConfigurationAndLifecycle(t *testing.T) {
	c := windowsConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true, result: StartResult{ID: "remote-thread", Job: "remote-run"}, thread: map[string]any{"status": "completed"}}
	r.Local = control
	r.Config = c
	task, err := r.Create("REMOTE-WINDOWS", c, false)
	if err != nil || task.Host != "windows" || task.Worktree != `C:/Users/NEON/.codex/worktrees/remote-project/remote-windows-`+r.State.Namespace[:8] {
		t.Fatal(task, err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Host != "windows" {
		t.Fatal(stored, err)
	}
	if _, err = r.Prepare(task.Ticket, c.Repo, c.Base); err != nil || !control.prepared {
		t.Fatal(err)
	}
	result, err := r.Run(task.Ticket, "inspect", "", "gpt-test")
	if err != nil || result.ID != "remote-thread" {
		t.Fatal(result, err)
	}
	status, err := r.Status(task.Ticket)
	if err != nil || status["t3_online"] != true {
		t.Fatal(status, err)
	}
	if err = r.StopTask(task.Ticket); err != nil || !control.stopped {
		t.Fatal(err)
	}
}

func TestWindowsManagedAppTaskStartsInsideSavedProject(t *testing.T) {
	c := windowsConfig()
	r, _, _ := setup(t)
	managedRoot := `C:/Users/NEON/.codex/worktrees/abcd/Project`
	control := &fakeLocalControl{managedRoot: managedRoot}
	r.Local = control
	r.Config = c
	task, err := r.CreateManagedAppTask("REMOTE-APP-PROJECT", c, "native-project-thread", managedRoot)
	if err != nil || task.Backend != "app" || task.Owner != "app" {
		t.Fatal(task, err)
	}
	var t3Rows int
	if err = r.State.DB.QueryRow("SELECT count(*) FROM t3_tasks WHERE ticket=?", task.Ticket).Scan(&t3Rows); err != nil || t3Rows != 0 {
		t.Fatal(t3Rows, err)
	}
	task, err = r.Prepare(task.Ticket, c.Repo, c.Base)
	if err != nil || !control.prepared || task.Phase != "active" || task.Worktree != managedRoot || task.AppThread != "native-project-thread" || task.Owner != "app" {
		t.Fatal(task, err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Backend != "app" || stored.AppThread != task.AppThread || stored.Worktree != managedRoot {
		t.Fatal(stored, err)
	}
	status, err := r.Status(task.Ticket)
	if err != nil || status["t3_online"] != false {
		t.Fatal(status, err)
	}
}

func TestWindowsManagedAppTaskRequiresRemoteProjectIdentity(t *testing.T) {
	c := windowsConfig()
	r, _, _ := setup(t)
	r.Config = c
	if _, err := r.CreateManagedAppTask("REMOTE-APP-NO-THREAD", c, "", `C:/Users/NEON/.codex/worktrees/abcd/Project`); err == nil {
		t.Fatal("managed App task accepted no native thread")
	}
	if _, err := r.CreateManagedAppTask("REMOTE-APP-ESCAPED", c, "native-thread", `C:/Users/NEON/Documents/escaped`); err == nil {
		t.Fatal("managed App task accepted a worktree outside the App root")
	}
}

func TestWindowsPrepareRequiresReadyWorkerBeforeChangingTask(t *testing.T) {
	c := windowsConfig()
	r, _, _ := setup(t)
	control := &fakeLocalControl{healthErr: errors.New("offline")}
	r.Local = control
	r.Config = c
	task, err := r.Create("REMOTE-HEALTH-GATE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Prepare(task.Ticket, c.Repo, c.Base); err == nil || !strings.Contains(err.Error(), "readiness preflight") {
		t.Fatal(err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Phase != "created" || stored.Repo != "" || control.prepared || control.healthCalls != 1 {
		t.Fatal(stored, control.prepared, control.healthCalls, err)
	}
}

func TestWindowsRecoverPrepareRequiresReadyWorkerBeforeMutation(t *testing.T) {
	c := windowsConfig()
	r, _, _ := setup(t)
	control := &fakeLocalControl{healthStatus: "starting"}
	r.Local = control
	r.Config = c
	task, err := r.Create("REMOTE-RECOVER-HEALTH-GATE", c, false)
	if err != nil {
		t.Fatal(err)
	}
	task.Repo = c.Repo
	task.Base = c.Base
	task.Branch = "remote-recover-health-gate-agent"
	task.Phase = "preparing"
	if err = r.State.Save(task); err != nil {
		t.Fatal(err)
	}
	if _, err = r.RecoverPrepare(task.Ticket); err == nil || !strings.Contains(err.Error(), "readiness preflight") {
		t.Fatal(err)
	}
	stored, err := r.State.Task(task.Ticket)
	if err != nil || stored.Phase != "preparing" || control.prepared || control.healthCalls != 1 {
		t.Fatal(stored, control.prepared, control.healthCalls, err)
	}
}

func TestWindowsTasksShareCodexProfileWithoutGlobalLease(t *testing.T) {
	c := windowsConfig()
	c.WindowsT3PortStart = 3784
	c.WindowsT3PortEnd = 3786
	r, _, _ := setup(t)
	control := &fakeLocalControl{online: true, result: StartResult{ID: "thread", Job: "run"}}
	r.Local = control
	r.Config = c
	first, err := r.Create("REMOTE-PROFILE-A", c, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Create("REMOTE-PROFILE-B", c, false)
	if err != nil {
		t.Fatal(err)
	}
	if first, err = r.Prepare(first.Ticket, c.Repo, c.Base); err != nil {
		t.Fatal(err)
	}
	if second, err = r.Prepare(second.Ticket, c.Repo, c.Base); err != nil {
		t.Fatal(err)
	}
	if err = r.State.Acquire(c.User, first.Ticket); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Run(second.Ticket, "inspect", "", "gpt-test"); err != nil {
		t.Fatal(err)
	}
	var leases int
	if err = r.State.DB.QueryRow("SELECT count(*) FROM leases WHERE user=?", c.User).Scan(&leases); err != nil || leases != 0 {
		t.Fatal(leases, err)
	}
	if _, err = r.Run(first.Ticket, "inspect", "", "gpt-test"); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsConfigurationRejectsIncompleteRemoteHost(t *testing.T) {
	c := windowsConfig()
	c.WindowsSSHTarget = ""
	if err := c.Validate(); err == nil {
		t.Fatal("missing SSH target accepted")
	}
	c = windowsConfig()
	c.WindowsRepoRoot = "/not/windows"
	if err := c.Validate(); err == nil {
		t.Fatal("non-Windows repository path accepted")
	}
	c = windowsConfig()
	c.WindowsT3PortStart = 3784
	if err := c.Validate(); err == nil {
		t.Fatal("partial Windows T3 port range accepted")
	}
}

func TestWindowsTasksAllocateDistinctPorts(t *testing.T) {
	c := windowsConfig()
	c.WindowsT3PortStart = 3784
	c.WindowsT3PortEnd = 3786
	r, _, _ := setup(t)
	r.Config = c
	first, err := r.Create("REMOTE-A", c, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Create("REMOTE-B", c, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.T3Port != 3784 || second.T3Port != 3785 {
		t.Fatal(first.T3Port, second.T3Port)
	}
}

func TestWindowsSinglePortConfigurationRejectsSecondTask(t *testing.T) {
	c := windowsConfig()
	r, _, _ := setup(t)
	r.Config = c
	if _, err := r.Create("REMOTE-A", c, false); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Create("REMOTE-B", c, false); err == nil || !strings.Contains(err.Error(), "no Windows T3 ports") {
		t.Fatal(err)
	}
}

func TestWindowsPreparingTaskMovesOffLegacyPortCollision(t *testing.T) {
	c := windowsConfig()
	c.WindowsT3PortStart = 3784
	c.WindowsT3PortEnd = 3786
	r, _, _ := setup(t)
	r.Config = c
	first, err := r.Create("REMOTE-A", c, false)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Ticket = "REMOTE-B"
	second.Name = "remote-b-" + r.State.Namespace[:8]
	second.Worktree = windowsJoin(c.WindowsWorktreeRoot, second.Name)
	second.T3Home = windowsJoin(c.WindowsStateRoot, second.Ticket)
	second.Phase = "preparing"
	second.Repo = c.Repo
	second.Base = c.Base
	second.Branch = "remote-b-agent"
	if err = r.State.Save(second); err != nil {
		t.Fatal(err)
	}
	updated, err := r.ensureWindowsTaskPort(second)
	if err != nil || updated.T3Port != 3785 {
		t.Fatal(updated.T3Port, err)
	}
}

func TestWindowsInstallerSecuresHostPrivateKeys(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("deploy", "install-windows-worker.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	installer := string(raw)
	for _, required := range []string{
		"ssh_host_*_key",
		"SetAccessRuleProtection($true, $false)",
		"SetOwner($systemSid)",
		"S-1-5-18",
		"S-1-5-32-544",
		"Get-Command git.exe",
		"`$env:Path = '$gitDirectory;' + `$env:Path",
		"SetupExecutablePath",
		"setup_args = @($SetupArguments)",
		"setup_paths = @($resolvedSetupPaths)",
		"T3PortStart",
		"t3_port_start = $T3PortStart",
		"t3_port_end = $T3PortEnd",
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("Windows installer does not enforce host-key ACL requirement %q", required)
		}
	}
}

func TestWindowsWorkerSetupPolicyIsHostPinned(t *testing.T) {
	policy := testWindowsWorkerPolicy()
	policy.SetupArgs = []string{"scripts/setup.sh"}
	if err := policy.Validate(); err == nil {
		t.Fatal("setup arguments without a host-pinned executable were accepted")
	}
	policy.SetupBin = `relative/bash.exe`
	if err := policy.Validate(); err == nil {
		t.Fatal("relative setup executable was accepted")
	}
	policy.SetupBin = `C:/Program Files/Git/bin/bash.exe`
	policy.SetupPaths = []string{`relative/node`}
	if err := policy.Validate(); err == nil {
		t.Fatal("relative setup PATH entry was accepted")
	}
	policy.SetupPaths = []string{`C:/Program Files/nodejs`}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsWorkerReturnsStructuredRemoteError(t *testing.T) {
	err := decodeWindowsWorkerError([]byte(`{"error":"cannot fetch configured local base"}`))
	if err == nil || err.Error() != "Windows worker: cannot fetch configured local base" {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{
		[]byte("ssh transport failed"),
		[]byte("warning\n{\"error\":\"cannot fetch configured local base\"}"),
		[]byte(`{"other":"value"}`),
	} {
		if err = decodeWindowsWorkerError(raw); err != nil {
			t.Fatalf("unstructured SSH stderr escaped: %v", err)
		}
	}
}

func TestWindowsSSHCommandUsesForcedWorkerRequest(t *testing.T) {
	c := windowsConfig()
	w := &windowsHost{config: c}
	request := windowsWorkerRequest{Operation: "online", Task: Task{Host: "windows", Ticket: "REMOTE"}}
	cmd, err := w.command(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if len(cmd.Args) != 21 || cmd.Args[len(cmd.Args)-1] != c.WindowsSSHTarget || !strings.Contains(joined, "StrictHostKeyChecking=yes") || !strings.Contains(joined, "ConnectTimeout=15") || !strings.Contains(joined, "ServerAliveCountMax=1") {
		t.Fatal(cmd.Args)
	}
	stdin, err := cmd.StdinPipe()
	if err == nil || stdin != nil {
		t.Fatal("preconfigured worker input unexpectedly exposed a pipe")
	}
	buffer := &bytes.Buffer{}
	if _, err = buffer.ReadFrom(cmd.Stdin); err != nil {
		t.Fatal(err)
	}
	decoded := windowsWorkerRequest{}
	if json.Unmarshal(buffer.Bytes(), &decoded) != nil || decoded.Operation != "online" || decoded.Task.Ticket != "REMOTE" {
		t.Fatal(string(buffer.Bytes()))
	}
}

func TestWindowsSSHCommandRejectsOptionInjection(t *testing.T) {
	c := windowsConfig()
	c.WindowsSSHTarget = "-oProxyCommand=attacker"
	if err := c.Validate(); err == nil {
		t.Fatal("SSH option accepted as a host alias")
	}
}

func TestWindowsWorkerRejectsUnscopedRequests(t *testing.T) {
	request := windowsWorkerRequest{Operation: "online", Task: Task{Host: "local", Ticket: "wrong-host"}}
	raw, _ := json.Marshal(request)
	if _, err := dispatchWindowsWorker(bytes.NewReader(raw), testWindowsWorkerPolicy()); err == nil || !strings.Contains(err.Error(), "wrong host") {
		t.Fatal(err)
	}
}

func TestWindowsWorkerHealthIsScopedAndVersioned(t *testing.T) {
	request := windowsWorkerRequest{Operation: "health"}
	raw, _ := json.Marshal(request)
	value, err := dispatchWindowsWorker(bytes.NewReader(raw), testWindowsWorkerPolicy())
	result, ok := value.(map[string]string)
	if err != nil || !ok || result["status"] != "ready" || result["version"] != Version || result["protocol"] != windowsWorkerProtocol {
		t.Fatal(value, err)
	}
}

func TestWindowsWorkerAcceptsOnlyPortsInsideConfiguredRange(t *testing.T) {
	policy := testWindowsWorkerPolicy()
	policy.T3Port = 0
	policy.T3PortStart = 3784
	policy.T3PortEnd = 3786
	request := windowsWorkerRequest{Operation: "unsupported", Task: Task{Host: "windows", SourceRoot: policy.RepoRoot, Worktree: policy.WorktreeRoot + "/task", T3Home: policy.StateRoot + "/task", T3Port: 3785}}
	raw, _ := json.Marshal(request)
	if _, err := dispatchWindowsWorker(bytes.NewReader(raw), policy); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatal(err)
	}
	request.Task.T3Port = 4000
	raw, _ = json.Marshal(request)
	if _, err := dispatchWindowsWorker(bytes.NewReader(raw), policy); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatal(err)
	}
}

func TestWindowsWorkerLoadsPrivatePolicyFile(t *testing.T) {
	policy := testWindowsWorkerPolicy()
	rawPolicy, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	policyPath := filepath.Join(t.TempDir(), "worker-policy.json")
	if err = os.WriteFile(policyPath, rawPolicy, 0600); err != nil {
		t.Fatal(err)
	}
	request, _ := json.Marshal(windowsWorkerRequest{Operation: "health"})
	value, err := runWindowsWorker(bytes.NewReader(request), []string{"--policy", policyPath})
	result, ok := value.(map[string]string)
	if err != nil || !ok || result["repo_root"] != normalizeWindowsPath(policy.RepoRoot) {
		t.Fatal(value, err)
	}
}

func TestWindowsSSHTransportRunsForcedWorkerProtocol(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable shell fixture is covered by Linux and macOS CI")
	}
	directory := t.TempDir()
	ssh := filepath.Join(directory, "ssh")
	script := "#!/bin/sh\nexec \"$CODEX_REMOTE_TEST_BINARY\" -test.run '^TestWindowsSSHWorkerHelper$'\n"
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_REMOTE_TEST_BINARY", os.Args[0])
	c := windowsConfig()
	c.WindowsSSHBin = ssh
	result, err := (&windowsHost{config: c}).Health()
	if err != nil || result["status"] != "ready" || result["version"] != Version {
		t.Fatal(result, err)
	}
}

func TestWindowsSSHWorkerHelper(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_TEST_BINARY") == "" {
		return
	}
	value, err := dispatchWindowsWorker(os.Stdin, testWindowsWorkerPolicy())
	if err != nil {
		os.Exit(1)
	}
	if err = json.NewEncoder(os.Stdout).Encode(value); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWindowsWorkerPolicyRejectsEscapedTaskPaths(t *testing.T) {
	policy := testWindowsWorkerPolicy()
	request := windowsWorkerRequest{Operation: "online", Task: Task{Host: "windows", SourceRoot: policy.RepoRoot, Worktree: `C:/Users/NEON/Documents/escaped-task`, T3Home: policy.StateRoot + "/task", T3Port: policy.T3Port}}
	raw, _ := json.Marshal(request)
	if _, err := dispatchWindowsWorker(bytes.NewReader(raw), policy); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatal(err)
	}
}

func TestWindowsWorkspaceAndBrowserPolicyMustBePinned(t *testing.T) {
	policy := testWindowsWorkerPolicy()
	policy.WorkspaceArgs = []string{"scripts/action.mjs"}
	if err := policy.Validate(); err == nil {
		t.Fatal("workspace arguments were accepted without a pinned executable")
	}
	policy.WorkspaceBin = `C:/Program Files/nodejs/node.exe`
	policy.WorkspacePaths = []string{`C:/Program Files/nodejs`}
	policy.BrowserBin = `C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe`
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	policy.BrowserBin = "relative/browser.exe"
	if err := policy.Validate(); err == nil {
		t.Fatal("relative browser executable was accepted")
	}
}

func TestWindowsWebLogsAreBounded(t *testing.T) {
	task := Task{T3Home: filepath.ToSlash(t.TempDir())}
	if err := os.WriteFile(windowsTaskControlPath(task, "web.log"), []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	value, err := windowsWebLogs(task, 2)
	if err != nil {
		t.Fatal(err)
	}
	lines, ok := value["lines"].([]string)
	if !ok || len(lines) != 2 || lines[0] != "three" {
		t.Fatal(value)
	}
	if _, err = windowsWebLogs(task, 1001); err == nil {
		t.Fatal("unbounded web log tail was accepted")
	}
}

func TestWindowsManagedTaskStopReleasesServiceLock(t *testing.T) {
	if os.Getenv("CODEX_REMOTE_MANAGED_TASK_HELPER") == "1" {
		time.Sleep(time.Hour)
		return
	}
	home := t.TempDir()
	task := Task{Ticket: "managed-stop", T3Home: home, T3Port: 49152}
	cmd := exec.Command(os.Args[0], "-test.run", "TestWindowsManagedTaskStopReleasesServiceLock")
	cmd.Env = append(os.Environ(), "CODEX_REMOTE_MANAGED_TASK_HELPER=1")
	served := make(chan error, 1)
	go func() { served <- serveWindowsTask(cmd, task) }()
	lockPath := windowsTaskControlPath(task, "serve.lock")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(lockPath); err == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	local := &localHost{}
	if err := stopWindowsTask(local, task); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("managed task did not stop")
	}
}

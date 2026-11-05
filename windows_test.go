package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
	c.WindowsStateRoot = `C:/Users/NEON/.codex/state/codex-remote-runner`
	c.WindowsT3Bin = `C:/Users/NEON/.t3/runtime/versions/0.1.7/t3.exe`
	c.WindowsCodexBin = `C:/Users/NEON/AppData/Local/OpenAI/Codex/bin/version/codex.exe`
	c.WindowsT3Port = 3784
	return c
}

func testWindowsWorkerPolicy() windowsWorkerPolicy {
	return windowsWorkerPolicy{
		RepoRoot:     `C:/Users/NEON/Documents/Project`,
		WorktreeRoot: `C:/Users/NEON/.codex/worktrees/remote-project`,
		StateRoot:    `C:/Users/NEON/.codex/state/codex-remote-runner`,
		T3Bin:        `C:/Users/NEON/.t3/runtime/versions/0.1.7/t3.exe`,
		CodexBin:     `C:/Users/NEON/AppData/Local/OpenAI/Codex/bin/version/codex.exe`,
		T3Port:       3784,
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
	} {
		if !strings.Contains(installer, required) {
			t.Fatalf("Windows installer does not enforce host-key ACL requirement %q", required)
		}
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
	if len(cmd.Args) != 13 || cmd.Args[len(cmd.Args)-1] != c.WindowsSSHTarget || !strings.Contains(strings.Join(cmd.Args, " "), "StrictHostKeyChecking=yes") {
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
	request := windowsWorkerRequest{Operation: "online", Task: Task{Host: "windows", SourceRoot: policy.RepoRoot, Worktree: `C:/Users/NEON/.codex/worktrees/other/task`, T3Home: policy.StateRoot + "/task", T3Port: policy.T3Port}}
	raw, _ := json.Marshal(request)
	if _, err := dispatchWindowsWorker(bytes.NewReader(raw), policy); err == nil || !strings.Contains(err.Error(), "escaped") {
		t.Fatal(err)
	}
}

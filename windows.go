package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

type windowsWorkerRequest struct {
	Operation  string `json:"operation"`
	Task       Task   `json:"task"`
	Base       string `json:"base,omitempty"`
	Branch     string `json:"branch,omitempty"`
	Prompt     string `json:"prompt,omitempty"`
	Model      string `json:"model,omitempty"`
	PublicURL  string `json:"public_url,omitempty"`
	ProjectID  string `json:"project_id,omitempty"`
	Mode       string `json:"mode,omitempty"`
	Path       string `json:"path,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	FullAccess bool   `json:"full_access,omitempty"`
	Tail       int    `json:"tail,omitempty"`
	Title      string `json:"title,omitempty"`
}

type windowsWorkerPolicy struct {
	RepoRoot        string   `json:"repo_root"`
	WorktreeRoot    string   `json:"worktree_root"`
	AppWorktreeRoot string   `json:"app_worktree_root"`
	StateRoot       string   `json:"state_root"`
	T3Bin           string   `json:"t3_bin"`
	CodexBin        string   `json:"codex_bin"`
	SetupBin        string   `json:"setup_bin,omitempty"`
	SetupArgs       []string `json:"setup_args,omitempty"`
	SetupPaths      []string `json:"setup_paths,omitempty"`
	WorkspaceBin    string   `json:"workspace_bin,omitempty"`
	WorkspaceArgs   []string `json:"workspace_args,omitempty"`
	WorkspacePaths  []string `json:"workspace_paths,omitempty"`
	BrowserBin      string   `json:"browser_bin,omitempty"`
	T3Port          int      `json:"t3_port"`
	T3PortStart     int      `json:"t3_port_start,omitempty"`
	T3PortEnd       int      `json:"t3_port_end,omitempty"`
}

type managedAppTask struct {
	ThreadID    string `json:"thread_id"`
	Worktree    string `json:"worktree"`
	Branch      string `json:"branch"`
	ProjectID   string `json:"project_id"`
	ProjectName string `json:"project_name"`
}

type windowsHost struct {
	config Config
}

const windowsWorkerProtocol = "2"

func (p windowsWorkerPolicy) localConfig() Config {
	c := defaultConfig()
	c.Host = "local"
	c.Backend = "t3"
	c.Provider = "codex"
	c.User = "windows-worker"
	c.LocalRepoRoot = normalizeWindowsPath(p.RepoRoot)
	c.LocalWorktreeRoot = normalizeWindowsPath(p.WorktreeRoot)
	c.LocalT3Bin = normalizeWindowsPath(p.T3Bin)
	c.LocalCodexBin = normalizeWindowsPath(p.CodexBin)
	start, _, _ := p.portRange()
	c.LocalT3Port = start
	return c
}

func (w *windowsHost) command(ctx context.Context, request windowsWorkerRequest) (*exec.Cmd, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if !sshAliasPattern.MatchString(w.config.WindowsSSHTarget) {
		return nil, errors.New("invalid Windows SSH configuration")
	}
	cmd := exec.CommandContext(ctx, w.config.WindowsSSHBin,
		"-T",
		"-o", "BatchMode=yes",
		"-o", "ClearAllForwardings=yes",
		"-o", "PermitLocalCommand=no",
		"-o", "ControlPath=none",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "ConnectTimeout=15",
		"-o", "ConnectionAttempts=1",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=1",
		w.config.WindowsSSHTarget,
	)
	cmd.Stdin = bytes.NewReader(raw)
	return cmd, nil
}

func (w *windowsHost) call(out any, request windowsWorkerRequest, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd, err := w.command(ctx, request)
	if err != nil {
		return err
	}
	stdout := &boundedOutput{limit: 4 << 20}
	stderr := &boundedOutput{limit: 64 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errors.New("Windows worker outcome uncertain; inspect before retrying")
		}
		if workerErr := decodeWindowsWorkerError(stderr.data); workerErr != nil {
			return workerErr
		}
		return errors.New("Windows worker operation failed; inspect the host privately")
	}
	if out != nil && json.Unmarshal(stdout.data, out) != nil {
		return errors.New("unexpected Windows worker response")
	}
	return nil
}

func decodeWindowsWorkerError(raw []byte) error {
	response := struct {
		Error string `json:"error"`
	}{}
	if json.Unmarshal(bytes.TrimSpace(raw), &response) != nil || response.Error == "" || len(response.Error) > 1024 {
		return nil
	}
	return errors.New("Windows worker: " + response.Error)
}

func (w *windowsHost) Prepare(t Task, base, branch string) error {
	return w.call(nil, windowsWorkerRequest{Operation: "prepare", Task: t, Base: base, Branch: branch}, 15*time.Minute)
}

func (w *windowsHost) PrepareManagedApp(t Task, base, branch string, recover bool) (managedAppTask, error) {
	result := managedAppTask{}
	operation := "prepare-managed-app"
	if recover {
		operation = "recover-managed-app"
	}
	err := w.call(&result, windowsWorkerRequest{Operation: operation, Task: t, Base: base, Branch: branch}, 20*time.Minute)
	return result, err
}

func (w *windowsHost) Archive(t Task, archived bool) (any, error) {
	operation := "archive"
	if !archived {
		operation = "unarchive"
	}
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: operation, Task: t}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Pin(t Task, pinned bool) (any, error) {
	operation := "pin"
	if !pinned {
		operation = "unpin"
	}
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: operation, Task: t}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Project(t Task, projectID string) (any, error) {
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: "project", Task: t, ProjectID: projectID}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Rename(t Task, title string) (any, error) {
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: "rename", Task: t, Title: title}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Projects(t Task) (any, error) {
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: "projects", Task: t}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Adopt(t Task, threadID string) (any, error) {
	var result any
	err := w.call(&result, windowsWorkerRequest{Operation: "adopt", Task: t, ThreadID: threadID}, 45*time.Second)
	return result, err
}

func (w *windowsHost) CreateAppThread(t Task, title, projectID string) (string, error) {
	result := ""
	err := w.call(&result, windowsWorkerRequest{Operation: "create-app-thread", Task: t, Title: title, ProjectID: projectID}, 45*time.Second)
	return result, err
}

func (w *windowsHost) AppRun(t Task, prompt string, fullAccess bool) (StartResult, error) {
	result := StartResult{}
	err := w.call(&result, windowsWorkerRequest{Operation: "app-run", Task: t, Prompt: prompt, FullAccess: fullAccess}, 4*time.Hour)
	return result, err
}

func (w *windowsHost) Health() (map[string]string, error) {
	result := map[string]string{}
	err := w.call(&result, windowsWorkerRequest{Operation: "health"}, 15*time.Second)
	if err == nil {
		start, end, rangeErr := w.config.windowsPortRange()
		if rangeErr != nil {
			return nil, rangeErr
		}
		expected := map[string]string{"repo_root": normalizeWindowsPath(w.config.WindowsRepoRoot), "worktree_root": normalizeWindowsPath(w.config.WindowsWorktreeRoot), "app_worktree_root": normalizeWindowsPath(w.config.windowsAppWorktreeRoot()), "state_root": normalizeWindowsPath(w.config.WindowsStateRoot), "version": Version, "protocol": windowsWorkerProtocol, "status": "ready"}
		if result["t3_port_start"] == "" && result["t3_port_end"] == "" && start == end {
			expected["t3_port"] = fmt.Sprint(start)
		} else {
			expected["t3_port_start"] = fmt.Sprint(start)
			expected["t3_port_end"] = fmt.Sprint(end)
		}
		for key, value := range expected {
			if !strings.EqualFold(result[key], value) {
				return nil, errors.New("Windows worker policy does not match controller configuration")
			}
		}
	}
	return result, err
}

func (w *windowsHost) Run(t Task, prompt, model string) (StartResult, error) {
	var result StartResult
	err := w.call(&result, windowsWorkerRequest{Operation: "run", Task: t, Prompt: prompt, Model: model}, 3*time.Minute)
	return result, err
}

func (w *windowsHost) Thread(t Task) (map[string]any, error) {
	result := map[string]any{}
	err := w.call(&result, windowsWorkerRequest{Operation: "thread", Task: t}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Pair(t Task, publicURL string) (string, error) {
	var result string
	err := w.call(&result, windowsWorkerRequest{Operation: "pair", Task: t, PublicURL: publicURL}, 45*time.Second)
	return result, err
}

func (w *windowsHost) Online(t Task) bool {
	var result bool
	return w.call(&result, windowsWorkerRequest{Operation: "online", Task: t}, 5*time.Second) == nil && result
}

func (w *windowsHost) Branch(t Task) (string, error) {
	var result string
	err := w.call(&result, windowsWorkerRequest{Operation: "branch", Task: t}, 30*time.Second)
	return result, err
}

func (w *windowsHost) Serve(t Task) error {
	request := windowsWorkerRequest{Operation: "serve", Task: t}
	cmd, err := w.command(context.Background(), request)
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	stop := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			close(stop)
		case <-stop:
		}
	}()
	err = runManagedProcessUntil(cmd, stop)
	select {
	case <-stop:
		if stopErr := w.Stop(t); stopErr != nil {
			return stopErr
		}
	default:
	}
	return err
}

func (w *windowsHost) Stop(t Task) error {
	return w.call(nil, windowsWorkerRequest{Operation: "stop", Task: t}, 20*time.Second)
}

func (w *windowsHost) PreparePreview(t Task) error {
	return w.call(nil, windowsWorkerRequest{Operation: "prepare-preview", Task: t}, 30*time.Minute)
}

func (w *windowsHost) ServeWeb(t Task, mode string) error {
	request := windowsWorkerRequest{Operation: "serve-web", Task: t, Mode: mode}
	cmd, err := w.command(context.Background(), request)
	if err != nil {
		return err
	}
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	stop := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			close(stop)
		case <-stop:
		}
	}()
	err = runManagedProcessUntil(cmd, stop)
	select {
	case <-stop:
		if stopErr := w.StopWeb(t); stopErr != nil {
			return stopErr
		}
	default:
	}
	return err
}

func (w *windowsHost) WebStatus(t Task, requestPath string) (map[string]any, error) {
	result := map[string]any{}
	err := w.call(&result, windowsWorkerRequest{Operation: "web-status", Task: t, Path: requestPath}, 45*time.Second)
	return result, err
}

func (w *windowsHost) StopWeb(t Task) error {
	return w.call(nil, windowsWorkerRequest{Operation: "stop-web", Task: t}, 20*time.Second)
}

func (w *windowsHost) WebLogs(t Task, tail int) (map[string]any, error) {
	result := map[string]any{}
	err := w.call(&result, windowsWorkerRequest{Operation: "web-logs", Task: t, Tail: tail}, 30*time.Second)
	return result, err
}

func (w *windowsHost) WebInspect(t Task, requestPath string) (webInspection, error) {
	result := webInspection{}
	err := w.call(&result, windowsWorkerRequest{Operation: "web-inspect", Task: t, Path: requestPath}, 2*time.Minute)
	return result, err
}

func runWindowsWorker(input io.Reader, args []string) (any, error) {
	if len(args) != 2 || args[0] != "--policy" || args[1] == "" {
		return nil, errors.New("Windows worker requires --policy")
	}
	file, err := os.Open(expandHome(args[1]))
	if err != nil {
		return nil, errors.New("cannot open Windows worker policy")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	policy := windowsWorkerPolicy{}
	if err = decoder.Decode(&policy); err != nil {
		return nil, errors.New("invalid Windows worker policy")
	}
	if err = policy.Validate(); err != nil {
		return nil, err
	}
	return dispatchWindowsWorker(input, policy)
}

func (p windowsWorkerPolicy) Validate() error {
	for name, value := range map[string]string{"repo_root": p.RepoRoot, "worktree_root": p.WorktreeRoot, "app_worktree_root": p.AppWorktreeRoot, "state_root": p.StateRoot, "t3_bin": p.T3Bin, "codex_bin": p.CodexBin} {
		if !windowsAbsolutePath(value) {
			return fmt.Errorf("Windows worker %s must be an absolute Windows path", name)
		}
	}
	start, end, err := p.portRange()
	if err != nil {
		return err
	}
	if start < 1024 || end > 65535 || start > end {
		return errors.New("Windows worker T3 port range must be between 1024 and 65535")
	}
	if p.SetupBin != "" && !windowsAbsolutePath(p.SetupBin) {
		return errors.New("Windows worker setup_bin must be an absolute Windows path")
	}
	if p.SetupBin == "" && (len(p.SetupArgs) != 0 || len(p.SetupPaths) != 0) {
		return errors.New("Windows worker setup arguments require setup_bin")
	}
	if len(p.SetupArgs) > 32 || len(p.SetupPaths) > 32 {
		return errors.New("Windows worker setup policy is too large")
	}
	for _, value := range p.SetupPaths {
		if !windowsAbsolutePath(value) {
			return errors.New("Windows worker setup_paths must be absolute Windows paths")
		}
	}
	if p.WorkspaceBin != "" && !windowsAbsolutePath(p.WorkspaceBin) {
		return errors.New("Windows worker workspace_bin must be an absolute Windows path")
	}
	if p.WorkspaceBin == "" && (len(p.WorkspaceArgs) != 0 || len(p.WorkspacePaths) != 0) {
		return errors.New("Windows worker workspace action arguments require workspace_bin")
	}
	if len(p.WorkspaceArgs) > 32 || len(p.WorkspacePaths) > 32 {
		return errors.New("Windows worker workspace action policy is too large")
	}
	for _, value := range p.WorkspacePaths {
		if !windowsAbsolutePath(value) {
			return errors.New("Windows worker workspace_paths must be absolute Windows paths")
		}
	}
	if p.BrowserBin != "" && !windowsAbsolutePath(p.BrowserBin) {
		return errors.New("Windows worker browser_bin must be an absolute Windows path")
	}
	return nil
}

func (p windowsWorkerPolicy) portRange() (int, int, error) {
	if p.T3PortStart == 0 && p.T3PortEnd == 0 {
		if p.T3Port < 1024 || p.T3Port > 65535 {
			return 0, 0, errors.New("Windows worker t3_port must be between 1024 and 65535")
		}
		return p.T3Port, p.T3Port, nil
	}
	if p.T3PortStart == 0 || p.T3PortEnd == 0 {
		return 0, 0, errors.New("Windows worker t3_port_start and t3_port_end must be configured together")
	}
	return p.T3PortStart, p.T3PortEnd, nil
}

func prepareManagedCodexAppTask(policy windowsWorkerPolicy, local *localHost, task Task, _ string, branch string) (managedAppTask, error) {
	if strings.TrimSpace(task.AppThread) == "" || strings.TrimSpace(task.Worktree) == "" {
		return managedAppTask{}, errors.New("managed Codex App task is missing its native thread or worktree")
	}
	if !windowsWithin(policy.AppWorktreeRoot, task.Worktree) {
		return managedAppTask{}, errors.New("managed Codex App task escaped the configured App worktree root")
	}
	repository, err := windowsGitRepositoryIdentity(policy.RepoRoot)
	if err != nil || !strings.EqualFold(repository, task.Repo) {
		return managedAppTask{}, errors.New("configured Codex Project repository does not match the requested task repository")
	}
	worktreeRepository, err := windowsGitRepositoryIdentity(task.Worktree)
	if err != nil || !strings.EqualFold(worktreeRepository, task.Repo) {
		return managedAppTask{}, errors.New("managed Codex App worktree repository does not match the requested task repository")
	}
	if _, err = local.Adopt(task, task.AppThread); err != nil {
		return managedAppTask{}, err
	}
	currentBranch, err := local.Branch(task)
	if err != nil {
		return managedAppTask{}, errors.New("cannot inspect Codex App-managed task branch")
	}
	if currentBranch == "" {
		if err = runLocalCommandWithTimeout(localGitCommandTimeout, nil, "git", "-C", task.Worktree, "switch", "-c", branch); err != nil {
			return managedAppTask{}, errors.New("cannot create the managed task branch")
		}
		currentBranch = branch
	}
	if currentBranch != branch {
		return managedAppTask{}, errors.New("Codex App-managed task already uses another branch")
	}
	if err = local.setup(task); err != nil {
		return managedAppTask{}, err
	}
	return managedAppTask{ThreadID: task.AppThread, Worktree: normalizeWindowsPath(task.Worktree), Branch: currentBranch}, nil
}

func windowsGitRepositoryIdentity(root string) (string, error) {
	out, err := exec.Command("git", "-C", filepath.FromSlash(root), "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSuffix(strings.TrimSpace(string(out)), ".git")
	lower := strings.ToLower(value)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if strings.HasPrefix(lower, prefix) {
			return value[len(prefix):], nil
		}
	}
	return "", errors.New("repository origin is not GitHub")
}

func dispatchWindowsWorker(input io.Reader, policy windowsWorkerPolicy) (any, error) {
	decoder := json.NewDecoder(io.LimitReader(input, 1<<20))
	decoder.DisallowUnknownFields()
	request := windowsWorkerRequest{}
	if err := decoder.Decode(&request); err != nil {
		return nil, errors.New("invalid Windows worker request")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("Windows worker accepts one request")
	}
	if request.Operation != "health" && request.Task.Host != "windows" {
		return nil, errors.New("Windows worker request has the wrong host")
	}
	if request.Operation != "health" {
		start, end, _ := policy.portRange()
		allowedWorktree := windowsWithin(policy.WorktreeRoot, request.Task.Worktree) || windowsWithin(policy.AppWorktreeRoot, request.Task.Worktree)
		if !strings.EqualFold(normalizeWindowsPath(request.Task.SourceRoot), normalizeWindowsPath(policy.RepoRoot)) || !allowedWorktree || !windowsWithin(policy.StateRoot, request.Task.T3Home) || request.Task.T3Port < start || request.Task.T3Port > end {
			return nil, errors.New("Windows worker request escaped its policy")
		}
	}
	workerConfig := policy.localConfig()
	local := &localHost{config: workerConfig, setupBin: policy.SetupBin, setupArgs: policy.SetupArgs, setupPaths: policy.SetupPaths}
	switch request.Operation {
	case "health":
		setup := "none"
		if policy.SetupBin != "" {
			setup = "configured"
		}
		start, end, _ := policy.portRange()
		workspace := "none"
		if policy.WorkspaceBin != "" {
			workspace = "configured"
		}
		return map[string]string{"status": "ready", "version": Version, "protocol": windowsWorkerProtocol, "repo_root": normalizeWindowsPath(policy.RepoRoot), "worktree_root": normalizeWindowsPath(policy.WorktreeRoot), "app_worktree_root": normalizeWindowsPath(policy.AppWorktreeRoot), "state_root": normalizeWindowsPath(policy.StateRoot), "t3_port": fmt.Sprint(start), "t3_port_start": fmt.Sprint(start), "t3_port_end": fmt.Sprint(end), "worktree_setup": setup, "workspace_actions": workspace}, nil
	case "prepare":
		return nil, local.Prepare(request.Task, request.Base, request.Branch)
	case "prepare-managed-app":
		return prepareManagedCodexAppTask(policy, local, request.Task, request.Base, request.Branch)
	case "recover-managed-app":
		return prepareManagedCodexAppTask(policy, local, request.Task, request.Base, request.Branch)
	case "archive":
		return local.Archive(request.Task, true)
	case "unarchive":
		return local.Archive(request.Task, false)
	case "pin":
		return local.Pin(request.Task, true)
	case "unpin":
		return local.Pin(request.Task, false)
	case "project":
		return local.Project(request.Task, request.ProjectID)
	case "rename":
		return local.Rename(request.Task, request.Title)
	case "projects":
		return local.Projects(request.Task)
	case "adopt":
		return local.Adopt(request.Task, request.ThreadID)
	case "create-app-thread":
		return local.CreateAppThread(request.Task, request.Title, request.ProjectID)
	case "app-run":
		return local.AppRun(request.Task, request.Prompt, request.FullAccess)
	case "run":
		return local.Run(request.Task, request.Prompt, request.Model)
	case "thread":
		return local.Thread(request.Task)
	case "pair":
		return local.Pair(request.Task, request.PublicURL)
	case "online":
		return local.Online(request.Task), nil
	case "branch":
		return local.Branch(request.Task)
	case "serve":
		if err := os.MkdirAll(request.Task.T3Home, 0700); err != nil {
			return nil, err
		}
		cmd := exec.Command(workerConfig.LocalT3Bin, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(request.Task.T3Port), "--base-dir", request.Task.T3Home)
		cmd.Dir = request.Task.Worktree
		cmd.Env = localEnvironment(workerConfig)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		return nil, serveWindowsTask(cmd, request.Task)
	case "stop":
		return nil, stopWindowsTask(local, request.Task)
	case "prepare-preview":
		return nil, runWorkspaceAction(policy, request.Task, "prepare-preview")
	case "serve-web":
		return nil, serveWindowsWeb(policy, request.Task, request.Mode)
	case "web-status":
		return windowsWebStatus(request.Task, request.Path)
	case "stop-web":
		return nil, stopWindowsManagedTask(request.Task, "web", nil)
	case "web-logs":
		return windowsWebLogs(request.Task, request.Tail)
	case "web-inspect":
		return inspectWindowsWeb(policy, request.Task, request.Path)
	default:
		return nil, errors.New("unsupported Windows worker operation")
	}
}

func windowsTaskControlPath(t Task, name string) string {
	return filepath.Join(filepath.FromSlash(t.T3Home), name)
}

func serveWindowsTask(cmd *exec.Cmd, t Task) error {
	return serveWindowsManagedTask(cmd, t, "serve")
}

func serveWindowsManagedTask(cmd *exec.Cmd, t Task, service string) error {
	lock := flock.New(windowsTaskControlPath(t, service+".lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return errors.New("cannot acquire Windows task service lock")
	}
	if !locked {
		return errors.New("Windows task T3 service is already starting or online")
	}
	defer lock.Unlock()
	stopPath := windowsTaskControlPath(t, service+".stop.request")
	if err = os.Remove(stopPath); err != nil && !os.IsNotExist(err) {
		return errors.New("cannot clear stale Windows task stop request")
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if _, statErr := os.Stat(stopPath); statErr == nil {
					close(stop)
					return
				}
			case <-done:
				return
			}
		}
	}()
	err = runManagedProcessUntil(cmd, stop)
	close(done)
	_ = os.Remove(stopPath)
	return err
}

func stopWindowsTask(local *localHost, t Task) error {
	return stopWindowsManagedTask(t, "serve", func() bool { return local.Online(t) })
}

func stopWindowsManagedTask(t Task, service string, externalOnline func() bool) error {
	lock := flock.New(windowsTaskControlPath(t, service+".lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return errors.New("cannot inspect Windows task service lock")
	}
	if locked {
		_ = lock.Unlock()
		if externalOnline != nil && externalOnline() {
			return errors.New("Windows task T3 is online outside worker management; stop it once before upgrading")
		}
		return nil
	}
	stopPath := windowsTaskControlPath(t, service+".stop.request")
	if err = os.WriteFile(stopPath, []byte("stop\n"), 0600); err != nil {
		return errors.New("cannot request Windows task stop")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
		locked, err = lock.TryLock()
		if err != nil {
			return errors.New("cannot verify Windows task stop")
		}
		if locked {
			_ = lock.Unlock()
			_ = os.Remove(stopPath)
			return nil
		}
	}
	return errors.New("Windows task stop is uncertain; inspect before retrying")
}

var cloudflareQuickTunnelPattern = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

func runWorkspaceAction(policy windowsWorkerPolicy, task Task, action string) error {
	if policy.WorkspaceBin == "" {
		return errors.New("workspace actions are not configured on the Windows worker")
	}
	cmd := exec.Command(policy.WorkspaceBin, append(append([]string{}, policy.WorkspaceArgs...), action)...)
	cmd.Dir = filepath.FromSlash(task.Worktree)
	cmd.Env = taskWorkspaceEnvironment(policy, task)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := runManagedProcess(cmd); err != nil {
		return errors.New("workspace action failed; inspect the host privately")
	}
	return nil
}

func serveWindowsWeb(policy windowsWorkerPolicy, task Task, mode string) error {
	if mode != "production" && mode != "preview" {
		return errors.New("web mode must be production or preview")
	}
	if policy.WorkspaceBin == "" {
		return errors.New("workspace actions are not configured on the Windows worker")
	}
	if err := os.MkdirAll(filepath.FromSlash(task.T3Home), 0700); err != nil {
		return err
	}
	logPath := windowsTaskControlPath(task, "web.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return errors.New("cannot open workspace web log")
	}
	defer logFile.Close()
	if err = os.WriteFile(windowsTaskControlPath(task, "web.mode"), []byte(mode+"\n"), 0600); err != nil {
		return errors.New("cannot record workspace web mode")
	}
	action := "run-" + mode
	cmd := exec.Command(policy.WorkspaceBin, append(append([]string{}, policy.WorkspaceArgs...), action)...)
	cmd.Dir = filepath.FromSlash(task.Worktree)
	cmd.Env = taskWorkspaceEnvironment(policy, task)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	return serveWindowsManagedTask(cmd, task, "web")
}

func taskWorkspaceEnvironment(policy windowsWorkerPolicy, task Task) []string {
	env := environmentWithPaths(os.Environ(), policy.WorkspacePaths)
	env = append(env,
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=safe.directory",
		"GIT_CONFIG_VALUE_0="+normalizeWindowsPath(task.Worktree),
	)
	return env
}

func windowsWebStatus(task Task, requestPath string) (map[string]any, error) {
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, "/") || strings.HasPrefix(requestPath, "//") || strings.ContainsAny(requestPath, "\r\n") {
		return nil, errors.New("web status path must be an absolute URL path")
	}
	lock := flock.New(windowsTaskControlPath(task, "web.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, errors.New("cannot inspect workspace web service lock")
	}
	if locked {
		_ = lock.Unlock()
		return map[string]any{"status": "offline"}, nil
	}
	raw, err := os.ReadFile(windowsTaskControlPath(task, "web.log"))
	if err != nil {
		return nil, errors.New("workspace web service has no readable log")
	}
	if len(raw) > 1<<20 {
		raw = raw[len(raw)-(1<<20):]
	}
	matches := cloudflareQuickTunnelPattern.FindAll(raw, -1)
	if len(matches) == 0 {
		return map[string]any{"status": "starting"}, nil
	}
	base, err := url.Parse(string(matches[len(matches)-1]))
	if err != nil || base.Scheme != "https" || !strings.HasSuffix(strings.ToLower(base.Hostname()), ".trycloudflare.com") {
		return nil, errors.New("workspace web log contained an invalid public URL")
	}
	modeRaw, _ := os.ReadFile(windowsTaskControlPath(task, "web.mode"))
	client := &http.Client{Timeout: 20 * time.Second}
	rootStatus, err := fetchPublicStatus(client, base.String()+"/")
	if err != nil {
		return map[string]any{"status": "starting", "mode": strings.TrimSpace(string(modeRaw)), "url": base.String(), "last_error": "public HTTPS root is not reachable"}, nil
	}
	journeyURL := base.ResolveReference(&url.URL{Path: requestPath}).String()
	journeyStatus := rootStatus
	if requestPath != "/" {
		journeyStatus, err = fetchPublicStatus(client, journeyURL)
		if err != nil {
			return map[string]any{"status": "starting", "mode": strings.TrimSpace(string(modeRaw)), "url": base.String(), "journey_url": journeyURL, "root_http_status": rootStatus, "last_error": "requested public HTTPS journey is not reachable"}, nil
		}
	}
	return map[string]any{"status": "online", "mode": strings.TrimSpace(string(modeRaw)), "url": base.String(), "journey_url": journeyURL, "root_http_status": rootStatus, "journey_http_status": journeyStatus}, nil
}

func fetchPublicStatus(client *http.Client, target string) (int, error) {
	request, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return response.StatusCode, errors.New("unexpected HTTP status")
	}
	return response.StatusCode, nil
}

func windowsWithin(root, target string) bool {
	root = strings.ToLower(path.Clean(normalizeWindowsPath(root)))
	target = strings.ToLower(path.Clean(normalizeWindowsPath(target)))
	return target != root && strings.HasPrefix(target, root+"/")
}

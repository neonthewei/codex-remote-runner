package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type LocalControl interface {
	Health() (map[string]string, error)
	Archive(Task, bool) (any, error)
	Pin(Task, bool) (any, error)
	Project(Task, string) (any, error)
	Rename(Task, string) (any, error)
	Projects(Task) (any, error)
	Adopt(Task, string) (any, error)
	CreateAppThread(Task, string, string) (string, error)
	AppRun(Task, string, bool) (StartResult, error)
	Prepare(Task, string, string) error
	Run(Task, string, string) (StartResult, error)
	Thread(Task) (map[string]any, error)
	Pair(Task, string) (string, error)
	Online(Task) bool
	Serve(Task) error
	Stop(Task) error
	PreparePreview(Task) error
	ServeWeb(Task, string) error
	WebStatus(Task, string) (map[string]any, error)
	StopWeb(Task) error
	WebLogs(Task, int) (map[string]any, error)
	WebInspect(Task, string) (webInspection, error)
	Branch(Task) (string, error)
}

type localHost struct {
	config     Config
	setupBin   string
	setupArgs  []string
	setupPaths []string
}

func (l *localHost) Health() (map[string]string, error) {
	return map[string]string{"status": "ready"}, nil
}

func (l *localHost) Archive(task Task, archived bool) (any, error) {
	return manageCodexAppArchive(l.config, task, archived)
}

func (l *localHost) Pin(task Task, pinned bool) (any, error) {
	return manageCodexAppPin(l.config, task, pinned)
}

func (l *localHost) Project(task Task, projectID string) (any, error) {
	return manageCodexAppProject(l.config, task, projectID)
}

func (l *localHost) Rename(task Task, title string) (any, error) {
	return manageCodexAppName(l.config, task, title)
}

func (l *localHost) Projects(task Task) (any, error) {
	return listCodexAppProjects(l.config, task)
}

func (l *localHost) Adopt(task Task, threadID string) (any, error) {
	return adoptCodexAppThread(l.config, task, threadID)
}

func (l *localHost) CreateAppThread(task Task, title, projectID string) (string, error) {
	return createCodexAppThread(l.config, task, title, projectID)
}

func (l *localHost) AppRun(task Task, prompt string, fullAccess bool) (StartResult, error) {
	return runCodexAppTurn(l.config, task, prompt, fullAccess)
}

const localGitCommandTimeout = 2 * time.Minute

var errLocalCommandTimedOut = errors.New("local command timed out")

func (r *Runner) localControl() LocalControl {
	if r.Local != nil {
		return r.Local
	}
	if r.Config.Host == "windows" {
		return &windowsHost{config: r.Config}
	}
	return &localHost{config: r.Config}
}

func (r *Runner) createLocal(ticket string, c Config) (Task, error) {
	if c.User == "" {
		return Task{}, errors.New("configure a local Codex profile name")
	}
	var exists int
	if err := r.State.DB.QueryRow("SELECT count(*) FROM tasks WHERE ticket=?", ticket).Scan(&exists); err != nil {
		return Task{}, err
	}
	if exists > 0 {
		t, err := r.State.Task(ticket)
		if err != nil {
			return t, err
		}
		if t.Host != c.Host || t.User != c.User {
			return t, errors.New("task already belongs to another host or profile")
		}
		return t, nil
	}
	root := ""
	source := ""
	t3Root := ""
	port := 0
	var err error
	if c.Host == "windows" {
		root = normalizeWindowsPath(c.WindowsWorktreeRoot)
		source = normalizeWindowsPath(c.WindowsRepoRoot)
		t3Root = normalizeWindowsPath(c.WindowsStateRoot)
		port, err = r.allocateWindowsPort(c, ticket)
		if err != nil {
			return Task{}, err
		}
	} else {
		root, err = filepath.Abs(expandHome(c.LocalWorktreeRoot))
		if err != nil {
			return Task{}, errors.New("cannot resolve local worktree root")
		}
		source, err = filepath.Abs(expandHome(c.LocalRepoRoot))
		if err != nil {
			return Task{}, errors.New("cannot resolve local repository root")
		}
		t3Root = filepath.Join(r.State.Root, "local-t3")
		port = c.LocalT3Port
	}
	name := strings.ToLower(ticket) + "-" + r.State.Namespace[:8]
	worktree := filepath.Join(root, name)
	t3Home := filepath.Join(t3Root, ticket)
	if c.Host == "windows" {
		worktree = windowsJoin(root, name)
		t3Home = windowsJoin(t3Root, ticket)
	} else if !within(root, worktree) {
		return Task{}, errors.New("worktree escaped configured root")
	}
	t := Task{Host: c.Host, Backend: "t3", Ticket: ticket, User: c.User, Provider: "codex", Org: c.Host, Name: name, Phase: "created", SourceRoot: source, Worktree: worktree, T3Home: t3Home, T3Port: port, Owner: "t3"}
	if err = r.State.Save(t); err != nil {
		return t, err
	}
	return t, nil
}

func (r *Runner) allocateWindowsPort(c Config, ticket string) (int, error) {
	start, end, err := c.windowsPortRange()
	if err != nil {
		return 0, err
	}
	rows, err := r.State.DB.Query("SELECT ticket,t3_port FROM windows_tasks")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var existing string
		var port int
		if err = rows.Scan(&existing, &port); err != nil {
			return 0, err
		}
		if existing != ticket {
			used[port] = true
		}
	}
	if err = rows.Err(); err != nil {
		return 0, err
	}
	for port := start; port <= end; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, errors.New("no Windows T3 ports are available in the configured range")
}

func (r *Runner) ensureWindowsTaskPort(t Task) (Task, error) {
	if t.Host != "windows" {
		return t, nil
	}
	start, end, err := r.Config.windowsPortRange()
	if err != nil {
		return t, err
	}
	var conflicts int
	if err = r.State.DB.QueryRow("SELECT count(*) FROM windows_tasks WHERE ticket<>? AND t3_port=?", t.Ticket, t.T3Port).Scan(&conflicts); err != nil {
		return t, err
	}
	if t.T3Port >= start && t.T3Port <= end && conflicts == 0 {
		return t, nil
	}
	if t.Phase != "created" && t.Phase != "preparing" {
		return t, errors.New("active Windows task port conflicts with the configured range; stop and reconcile it before continuing")
	}
	port, err := r.allocateWindowsPort(r.Config, t.Ticket)
	if err != nil {
		return t, err
	}
	t.T3Port = port
	return t, r.State.Save(t)
}

func isWorktreeHost(host string) bool {
	return host == "local" || host == "windows"
}

func normalizeWindowsPath(value string) string {
	return strings.ReplaceAll(value, "\\", "/")
}

func windowsJoin(root string, name string) string {
	return strings.TrimRight(normalizeWindowsPath(root), "/") + "/" + strings.Trim(name, "/")
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (r *Runner) prepareLocal(t Task, repo, base string) (Task, error) {
	if t.Repo != "" {
		if t.Repo != repo || t.Base != base {
			return t, errors.New("task already owns another repo or base")
		}
		if t.Phase == "active" {
			return t, nil
		}
		return t, errors.New("local preparation is uncertain; run recover-prepare")
	}
	if err := r.requireWindowsWorkerReady(t); err != nil {
		return t, err
	}
	var err error
	if t, err = r.ensureWindowsTaskPort(t); err != nil {
		return t, err
	}
	t.Repo = repo
	t.Base = base
	t.Branch = strings.ToLower(t.Ticket) + "-agent"
	t.Phase = "preparing"
	if err := r.State.Save(t); err != nil {
		return t, err
	}
	if err := r.localControl().Prepare(t, base, t.Branch); err != nil {
		return t, err
	}
	t.Phase = "active"
	return t, r.State.Save(t)
}

func (r *Runner) recoverPrepareLocal(t Task) (Task, error) {
	if t.Phase != "preparing" || t.Repo == "" || t.Branch == "" {
		return t, errors.New("task is not awaiting local preparation recovery")
	}
	if err := r.requireWindowsWorkerReady(t); err != nil {
		return t, err
	}
	var portErr error
	if t, portErr = r.ensureWindowsTaskPort(t); portErr != nil {
		return t, portErr
	}
	branch, err := r.localControl().Branch(t)
	if err == nil && branch != t.Branch {
		return t, errors.New("local worktree does not match recorded task branch")
	}
	if prepareErr := r.localControl().Prepare(t, t.Base, t.Branch); prepareErr != nil {
		return t, errors.New("local worktree preparation did not complete")
	}
	branch, err = r.localControl().Branch(t)
	if err != nil || branch != t.Branch {
		return t, errors.New("local worktree does not match recorded task branch")
	}
	t.Phase = "active"
	return t, r.State.Save(t)
}

func (r *Runner) requireWindowsWorkerReady(t Task) error {
	if t.Host != "windows" {
		return nil
	}
	result, err := r.localControl().Health()
	if err != nil || result["status"] != "ready" {
		return errors.New("Windows worker readiness preflight failed; wait for check-host to report ready before retrying")
	}
	return nil
}

func (r *Runner) runLocal(t Task, prompt, model string) (StartResult, error) {
	var result StartResult
	if t.Phase != "active" {
		return result, errors.New("local task needs recovery before execution")
	}
	if t.Owner != "t3" {
		return result, errors.New("local task is owned by the Codex app; hand it back to t3 before running")
	}
	if !r.localControl().Online(t) {
		return result, errors.New("local T3 server is offline; run serve-task for this task")
	}
	if err := r.State.ClearWorktreeLeases(t.User); err != nil {
		return result, err
	}
	t.Phase = "starting-turn"
	if err := r.State.Save(t); err != nil {
		return result, err
	}
	if _, err := r.State.DB.Exec("INSERT INTO turns(ticket,conversation,job,mode,status) VALUES(?,?,?,'local','uncertain') ON CONFLICT(ticket) DO UPDATE SET conversation=excluded.conversation,job='',mode='local',status='uncertain',note=''", t.Ticket, t.Conversation, ""); err != nil {
		return result, err
	}
	result, err := r.localControl().Run(t, prompt, model)
	if err != nil {
		return result, err
	}
	if result.ID == "" || result.Job == "" {
		return result, errors.New("local T3 start response missing thread or run; reconcile before retrying")
	}
	t.Conversation = result.ID
	t.Phase = "active"
	if err = r.State.Save(t); err != nil {
		return result, err
	}
	_, err = r.State.DB.Exec("UPDATE turns SET conversation=?,job=?,status='active',note='' WHERE ticket=?", result.ID, result.Job, t.Ticket)
	return result, err
}

func (r *Runner) recoverTurnLocal(t Task, conversation string) (Task, error) {
	if t.Phase != "starting-turn" || conversation == "" || (t.Conversation != "" && t.Conversation != conversation) {
		return t, errors.New("select the original local T3 thread for the uncertain turn")
	}
	if !r.localControl().Online(t) {
		return t, errors.New("local T3 server is offline; start it before recovery")
	}
	selected := t
	selected.Conversation = conversation
	value, err := r.localControl().Thread(selected)
	if err != nil {
		return t, err
	}
	thread, ok := value["thread"].(map[string]any)
	if !ok || thread["threadId"] != conversation || thread["providerInstanceId"] != "codex" {
		return t, errors.New("selected local T3 thread does not match the Codex task")
	}
	job, _ := thread["latestRunId"].(string)
	if job == "" {
		return t, errors.New("selected local T3 thread has no verifiable run")
	}
	t.Conversation = conversation
	t.Phase = "active"
	if err = r.State.Save(t); err != nil {
		return t, err
	}
	_, err = r.State.DB.Exec("UPDATE turns SET conversation=?,job=?,status='active',note='' WHERE ticket=?", conversation, job, t.Ticket)
	return t, err
}

func (r *Runner) statusLocal(t Task) (map[string]any, error) {
	result := map[string]any{"task": t, "t3_online": r.localControl().Online(t), "owner": t.Owner, "worktree": t.Worktree}
	if !result["t3_online"].(bool) || t.Conversation == "" {
		return result, nil
	}
	thread, err := r.localControl().Thread(t)
	if err != nil {
		result["thread_unavailable"] = true
		return result, nil
	}
	result["thread"] = thread
	return result, nil
}

func (r *Runner) ServeTask(ticket string) error {
	t, err := r.State.Task(ticket)
	if err != nil {
		return err
	}
	if !isWorktreeHost(t.Host) {
		return errors.New("serve-task requires a worktree task")
	}
	return r.localControl().Serve(t)
}

func (r *Runner) StopTask(ticket string) error {
	t, err := r.State.Task(ticket)
	if err != nil {
		return err
	}
	if t.Host != "windows" {
		return errors.New("stop-task currently requires a Windows worktree task")
	}
	return r.localControl().Stop(t)
}

func (r *Runner) ManageCodexAppTask(ticket, action, projectID string) (any, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return nil, err
	}
	if !isWorktreeHost(task.Host) {
		return nil, errors.New("Codex App task management requires a worktree task")
	}
	switch action {
	case "archive-task":
		return r.localControl().Archive(task, true)
	case "unarchive-task":
		return r.localControl().Archive(task, false)
	case "pin-task":
		return r.localControl().Pin(task, true)
	case "unpin-task":
		return r.localControl().Pin(task, false)
	case "project-task":
		return r.localControl().Project(task, projectID)
	case "rename-task":
		return r.localControl().Rename(task, projectID)
	case "list-projects":
		return r.localControl().Projects(task)
	default:
		return nil, errors.New("unsupported Codex App task operation")
	}
}

func (r *Runner) AdoptCodexAppThread(ticket, threadID string) (Task, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return task, err
	}
	if !isWorktreeHost(task.Host) || task.Phase != "active" {
		return task, errors.New("adopt-thread requires an active worktree task")
	}
	if r.localControl().Online(task) {
		return task, errors.New("stop serve-task before adopting a Codex App thread")
	}
	if _, err = r.localControl().Adopt(task, threadID); err != nil {
		return task, err
	}
	task.AppThread = threadID
	task.Owner = "app"
	return task, r.State.Save(task)
}

func (r *Runner) CreateCodexAppThread(ticket, title, projectID string) (Task, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return task, err
	}
	if !isWorktreeHost(task.Host) || task.Phase != "active" || task.AppThread != "" {
		return task, errors.New("create-app-thread requires an active task without a recorded App thread")
	}
	if r.localControl().Online(task) {
		return task, errors.New("stop serve-task before creating a native Codex App thread")
	}
	threadID, err := r.localControl().CreateAppThread(task, title, projectID)
	if err != nil {
		return task, err
	}
	task.AppThread = threadID
	task.Owner = "app"
	return task, r.State.Save(task)
}

func (r *Runner) RunCodexAppThread(ticket, prompt string, fullAccess bool) (StartResult, error) {
	task, err := r.State.Task(ticket)
	if err != nil {
		return StartResult{}, err
	}
	if !isWorktreeHost(task.Host) || task.Phase != "active" || task.Owner != "app" || task.AppThread == "" {
		return StartResult{}, errors.New("app-run requires an adopted active Codex App thread")
	}
	if r.localControl().Online(task) {
		return StartResult{}, errors.New("stop serve-task before running an adopted Codex App thread")
	}
	return r.localControl().AppRun(task, prompt, fullAccess)
}

func (r *Runner) HandoffLocal(ticket, owner string) (Task, error) {
	t, err := r.State.Task(ticket)
	if err != nil {
		return t, err
	}
	if !isWorktreeHost(t.Host) {
		return t, errors.New("handoff requires a worktree task")
	}
	if owner != "app" && owner != "t3" {
		return t, errors.New("handoff target must be app or t3")
	}
	online := r.localControl().Online(t)
	if owner == "app" && online {
		return t, errors.New("stop serve-task before handing the single writer to the Codex app")
	}
	if owner == "t3" && !online {
		return t, errors.New("start serve-task before handing the single writer to T3")
	}
	t.Owner = owner
	return t, r.State.Save(t)
}

func (l *localHost) Prepare(t Task, base, branch string) error {
	root, err := filepath.Abs(expandHome(l.config.LocalWorktreeRoot))
	if err != nil || !within(root, t.Worktree) {
		return errors.New("local worktree escaped configured root")
	}
	info, statErr := os.Stat(t.Worktree)
	if statErr == nil {
		if !info.IsDir() {
			return errors.New("local worktree path already exists")
		}
		existingBranch, branchErr := l.Branch(t)
		if branchErr != nil || existingBranch != branch {
			return errors.New("local worktree path already exists with another branch")
		}
		return l.setup(t)
	}
	if !os.IsNotExist(statErr) {
		return errors.New("cannot inspect local worktree path")
	}
	if err := runLocalCommandWithTimeout(localGitCommandTimeout, nil, "git", "-C", t.SourceRoot, "fetch", "origin", base); err != nil {
		return errors.New("cannot fetch configured local base")
	}
	if err := os.MkdirAll(filepath.Dir(t.Worktree), 0700); err != nil {
		return err
	}
	if err := runLocalCommandWithTimeout(localGitCommandTimeout, nil, "git", "-C", t.SourceRoot, "worktree", "add", "-b", branch, t.Worktree, "origin/"+base); err != nil {
		return errors.New("cannot create local task worktree")
	}
	return l.setup(t)
}

func (l *localHost) setup(t Task) error {
	if l.setupBin == "" {
		return nil
	}
	cmd := exec.Command(l.setupBin, l.setupArgs...)
	cmd.Dir = t.Worktree
	cmd.Env = environmentWithPaths(os.Environ(), l.setupPaths)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := runManagedProcess(cmd); err != nil {
		return errors.New("local worktree setup failed; inspect the host privately")
	}
	return nil
}

func (l *localHost) Run(t Task, prompt, model string) (StartResult, error) {
	var result StartResult
	if t.Conversation == "" {
		if model == "" {
			return result, errors.New("first local T3 run requires --model")
		}
		if err := l.ensureProject(t); err != nil {
			return result, err
		}
		raw, err := l.t3(strings.NewReader(prompt), t, "thread", "start", "--base-dir", t.T3Home, "--provider", "codex", "--model", model, "--title", t.Ticket, "--json", t.Worktree, "-")
		if err != nil {
			return result, err
		}
		return decodeLocalStart(raw)
	}
	raw, err := l.t3(strings.NewReader(prompt), t, "thread", "send", "--base-dir", t.T3Home, "--json", t.Conversation, "-")
	if err != nil {
		return result, err
	}
	return decodeLocalStart(raw)
}

func decodeLocalStart(raw []byte) (StartResult, error) {
	var value struct {
		Thread string `json:"threadId"`
		Run    string `json:"runId"`
	}
	if json.Unmarshal(raw, &value) != nil || value.Thread == "" || value.Run == "" {
		return StartResult{}, errors.New("unexpected local T3 response; reconcile before retrying")
	}
	return StartResult{ID: value.Thread, Job: value.Run}, nil
}

func (l *localHost) Thread(t Task) (map[string]any, error) {
	raw, err := l.t3(nil, t, "thread", "show", "--base-dir", t.T3Home, "--messages", "4", "--json", t.Conversation)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return nil, errors.New("unexpected local T3 status")
	}
	return value, nil
}

func (l *localHost) Pair(t Task, publicURL string) (string, error) {
	raw, err := l.t3(nil, t, "auth", "pairing", "create", "--base-dir", t.T3Home, "--ttl", "5m", "--base-url", publicURL, "--json")
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func (l *localHost) Online(t Task) bool {
	client := http.Client{Timeout: 750 * time.Millisecond}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/.well-known/t3/environment", t.T3Port))
	if err != nil {
		return false
	}
	response.Body.Close()
	return response.StatusCode >= 200 && response.StatusCode < 300
}

func (l *localHost) Serve(t Task) error {
	if err := os.MkdirAll(t.T3Home, 0700); err != nil {
		return err
	}
	cmd := exec.Command(l.config.LocalT3Bin, "serve", "--host", "127.0.0.1", "--port", fmt.Sprint(t.T3Port), "--base-dir", t.T3Home)
	cmd.Dir = t.Worktree
	cmd.Env = localEnvironment(l.config)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return runManagedProcess(cmd)
}

func (l *localHost) Stop(Task) error {
	return errors.New("managed stop is not available for this local host")
}

func (l *localHost) PreparePreview(Task) error {
	return errors.New("workspace actions are not configured for this local host")
}

func (l *localHost) ServeWeb(Task, string) error {
	return errors.New("workspace actions are not configured for this local host")
}

func (l *localHost) WebStatus(Task, string) (map[string]any, error) {
	return nil, errors.New("workspace actions are not configured for this local host")
}

func (l *localHost) StopWeb(Task) error {
	return errors.New("workspace actions are not configured for this local host")
}

func (l *localHost) WebLogs(Task, int) (map[string]any, error) {
	return nil, errors.New("workspace actions are not configured for this local host")
}

func (l *localHost) WebInspect(Task, string) (webInspection, error) {
	return webInspection{}, errors.New("browser inspection is not configured for this local host")
}

func (l *localHost) Branch(t Task) (string, error) {
	out, err := exec.Command("git", "-C", t.Worktree, "branch", "--show-current").Output()
	if err != nil {
		return "", errors.New("cannot inspect worktree branch")
	}
	return strings.TrimSpace(string(out)), nil
}

func (l *localHost) ensureProject(t Task) error {
	_, err := l.t3(nil, t, "thread", "list", "--base-dir", t.T3Home, "--project", t.Worktree, "--json")
	if err == nil {
		return nil
	}
	_, err = l.t3(nil, t, "project", "add", "--base-dir", t.T3Home, "--title", t.Ticket, t.Worktree)
	if err != nil {
		return errors.New("cannot register local worktree with T3")
	}
	return nil
}

func (l *localHost) t3(input io.Reader, t Task, args ...string) ([]byte, error) {
	cmd := exec.Command(l.config.LocalT3Bin, args...)
	cmd.Dir = t.Worktree
	cmd.Env = localEnvironment(l.config)
	cmd.Stdin = input
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, errors.New("local T3 operation failed; inspect the task privately")
	}
	return stdout.Bytes(), nil
}

func localEnvironment(c Config) []string {
	paths := []string{}
	if filepath.IsAbs(c.LocalCodexBin) {
		paths = append(paths, filepath.Dir(c.LocalCodexBin))
	}
	return environmentWithPaths(os.Environ(), paths)
}

func environmentWithPaths(env, paths []string) []string {
	if len(paths) == 0 {
		return env
	}
	prefix := strings.Join(paths, string(os.PathListSeparator))
	for index, value := range env {
		if strings.HasPrefix(strings.ToUpper(value), "PATH=") {
			env[index] = "PATH=" + prefix + string(os.PathListSeparator) + value[len("PATH="):]
			return env
		}
	}
	env = append(env, "PATH="+prefix)
	return env
}

func runLocalCommandWithTimeout(timeout time.Duration, input io.Reader, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = input
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	stop := make(chan struct{})
	timedOut := make(chan struct{}, 1)
	timer := time.AfterFunc(timeout, func() {
		timedOut <- struct{}{}
		close(stop)
	})
	err := runManagedProcessUntil(cmd, stop)
	if !timer.Stop() {
		<-timedOut
		return errLocalCommandTimedOut
	}
	return err
}

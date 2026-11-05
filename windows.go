package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"
)

type windowsWorkerRequest struct {
	Operation string `json:"operation"`
	Task      Task   `json:"task"`
	Base      string `json:"base,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	Model     string `json:"model,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
}

type windowsWorkerPolicy struct {
	RepoRoot     string `json:"repo_root"`
	WorktreeRoot string `json:"worktree_root"`
	StateRoot    string `json:"state_root"`
	T3Bin        string `json:"t3_bin"`
	CodexBin     string `json:"codex_bin"`
	T3Port       int    `json:"t3_port"`
}

type windowsHost struct {
	config Config
}

const windowsWorkerProtocol = "1"

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
	c.LocalT3Port = p.T3Port
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
	return w.call(nil, windowsWorkerRequest{Operation: "prepare", Task: t, Base: base, Branch: branch}, 3*time.Minute)
}

func (w *windowsHost) Health() (map[string]string, error) {
	result := map[string]string{}
	err := w.call(&result, windowsWorkerRequest{Operation: "health"}, 15*time.Second)
	if err == nil {
		expected := map[string]string{"repo_root": normalizeWindowsPath(w.config.WindowsRepoRoot), "worktree_root": normalizeWindowsPath(w.config.WindowsWorktreeRoot), "state_root": normalizeWindowsPath(w.config.WindowsStateRoot), "t3_port": fmt.Sprint(w.config.WindowsT3Port), "version": Version, "protocol": windowsWorkerProtocol, "status": "ready"}
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
	return runManagedProcess(cmd)
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
	for name, value := range map[string]string{"repo_root": p.RepoRoot, "worktree_root": p.WorktreeRoot, "state_root": p.StateRoot, "t3_bin": p.T3Bin, "codex_bin": p.CodexBin} {
		if !windowsAbsolutePath(value) {
			return fmt.Errorf("Windows worker %s must be an absolute Windows path", name)
		}
	}
	if p.T3Port < 1024 || p.T3Port > 65535 {
		return errors.New("Windows worker t3_port must be between 1024 and 65535")
	}
	return nil
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
		if !strings.EqualFold(normalizeWindowsPath(request.Task.SourceRoot), normalizeWindowsPath(policy.RepoRoot)) || !windowsWithin(policy.WorktreeRoot, request.Task.Worktree) || !windowsWithin(policy.StateRoot, request.Task.T3Home) || request.Task.T3Port != policy.T3Port {
			return nil, errors.New("Windows worker request escaped its policy")
		}
	}
	workerConfig := policy.localConfig()
	local := &localHost{config: workerConfig}
	switch request.Operation {
	case "health":
		return map[string]string{"status": "ready", "version": Version, "protocol": windowsWorkerProtocol, "repo_root": normalizeWindowsPath(policy.RepoRoot), "worktree_root": normalizeWindowsPath(policy.WorktreeRoot), "state_root": normalizeWindowsPath(policy.StateRoot), "t3_port": fmt.Sprint(policy.T3Port)}, nil
	case "prepare":
		return nil, local.Prepare(request.Task, request.Base, request.Branch)
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
		return nil, runManagedProcess(cmd)
	default:
		return nil, errors.New("unsupported Windows worker operation")
	}
}

func windowsWithin(root, target string) bool {
	root = strings.ToLower(path.Clean(normalizeWindowsPath(root)))
	target = strings.ToLower(path.Clean(normalizeWindowsPath(target)))
	return target != root && strings.HasPrefix(target, root+"/")
}

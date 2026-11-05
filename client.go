package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Box struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Org      string            `json:"orgId"`
	State    string            `json:"state"`
	Vault    bool              `json:"vault"`
	Metadata map[string]string `json:"metadata"`
}
type Conversation struct {
	ID       string `json:"conversationId"`
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Job      string `json:"jobId"`
	ExitCode *int   `json:"exitCode"`
}
type Process struct {
	Command string `json:"command"`
}
type StartResult struct {
	ID   string `json:"conversationId"`
	Job  string `json:"jobId"`
	Turn int    `json:"turn"`
	URL  string `json:"url,omitempty"`
}
type Client interface {
	Call(any, ...string) error
	Prompt(any, string, ...string) error
	Shell(string, string) (string, error)
	Credentials(string, []byte) ([]byte, error)
}

type Instabox struct {
	Timeout   time.Duration
	MaxOutput int
}

type boundedOutput struct {
	data  []byte
	limit int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-len(b.data) {
		return 0, errors.New("output limit exceeded")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func (i Instabox) Call(out any, args ...string) error {
	return i.call(out, nil, args...)
}

func (i Instabox) Prompt(out any, prompt string, args ...string) error {
	return i.call(out, strings.NewReader(prompt), append(args, "-")...)
}

func (i Instabox) call(out any, input io.Reader, args ...string) error {
	argv := append([]string{}, args...)
	pos := len(argv)
	for n, s := range argv {
		if s == "--" {
			pos = n
			break
		}
	}
	argv = append(argv, "")
	copy(argv[pos+1:], argv[pos:])
	argv[pos] = "--json"
	timeout := i.Timeout
	if timeout == 0 {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "instabox", argv...)
	cmd.Stdin = input
	limit := i.MaxOutput
	if limit == 0 {
		limit = 64 << 20
	}
	output := &boundedOutput{limit: limit}
	cmd.Stdout = output
	e := cmd.Run()
	raw := output.data
	if ctx.Err() != nil {
		return errors.New("Instabox outcome uncertain; reconcile before retrying")
	}
	if e != nil {
		return errors.New("Instabox operation failed; inspect task state privately")
	}
	if out != nil && json.Unmarshal(raw, out) != nil {
		return errors.New("unexpected Instabox response; reconcile before retrying")
	}
	return nil
}
func (i Instabox) Shell(box, command string) (string, error) {
	var r struct {
		Code   *int   `json:"exitCode"`
		Stdout string `json:"stdout"`
	}
	if e := i.Call(&r, "exec", box, "--", "bash", "-lc", command); e != nil {
		return "", e
	}
	if r.Code == nil || *r.Code != 0 {
		return "", errors.New("remote prerequisite failed; inspect VM privately")
	}
	return r.Stdout, nil
}
func (i Instabox) copy(source, target string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if e := exec.CommandContext(ctx, "instabox", "cp", source, target).Run(); e != nil {
		return errors.New("credential transfer failed; values were not logged")
	}
	return nil
}
func (i Instabox) Credentials(box string, value []byte) ([]byte, error) {
	dir, e := os.MkdirTemp("", "codex-remote-auth-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "auth.json")
	if e = os.WriteFile(p, nil, 0600); e != nil {
		return nil, e
	}
	if value == nil {
		if e = i.copy(box+":/data/home/.codex/auth.json", p); e != nil {
			return nil, e
		}
		raw, e := os.ReadFile(p)
		if e != nil {
			return nil, errors.New("cannot read transferred auth")
		}
		return validateAuth(raw)
	}
	if _, e = validateAuth(value); e != nil {
		return nil, e
	}
	if e = os.WriteFile(p, value, 0600); e != nil {
		return nil, e
	}
	if _, e = i.Shell(box, "umask 077; mkdir -p /data/home/.codex; chmod 700 /data/home/.codex"); e != nil {
		return nil, e
	}
	if e = i.copy(p, box+":/data/home/.codex/auth.json"); e != nil {
		return nil, e
	}
	_, e = i.Shell(box, "chmod 600 /data/home/.codex/auth.json; printf 'cli_auth_credentials_store = \"file\"\\n' > /data/home/.codex/config.toml")
	return nil, e
}

// Follow drains native JSONL without buffering a transcript or applying the
// short request timeout. EOF is only a wake-up signal, never completion proof.
type eventFollower interface {
	Follow(context.Context, string, string) error
}

func (i Instabox) Follow(ctx context.Context, box, conversation string) error {
	cmd := exec.CommandContext(ctx, "instabox", "agent", "events", box, "-c", conversation, "-f")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errors.New("native event follow ended; reconcile task status")
	}
	return nil
}

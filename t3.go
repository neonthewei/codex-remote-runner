package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func validT3Origin(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && !strings.ContainsAny(raw, "\r\n")
}

// Setup is limited to a dedicated controller-owned task VM. The provider and
// all of its children inherit this service's cgroup. No public guest port.
func (r *Runner) setupT3(t Task) error {
	if _, e := r.owned(t); e != nil {
		return e
	}
	installer, ok := r.Client.(interface{ InstallT3Guest(string, string) error })
	if !ok {
		return errors.New("client cannot install T3 guest guard")
	}
	if e := installer.InstallT3Guest(t.Box, r.Config.GuestBinary); e != nil {
		return e
	}
	command := `set -eu
if ! test -x ` + t3Bin + `; then t3 update ` + t3Version + ` --yes >/dev/null; fi
test -x ` + t3Bin + `
sudo -n systemctl disable --now t3-instabox.service >/dev/null 2>&1 || true
umask 077
mkdir -p ` + t3Home + `
test ! -L ` + t3Home + `
chmod 700 ` + t3Home + `
unit=$(mktemp)
trap 'rm -f "$unit"' EXIT
cat > "$unit" <<UNIT
[Unit]
Description=Instabox task T3 Code
After=network-online.target
[Service]
User=$(id -un)
Environment=HOME=/data/home
Environment=PATH=/data/home/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/bin/bash -lc "exec ` + t3Bin + ` serve --host 127.0.0.1 --port 3774 --base-dir ` + t3Home + `"
Restart=no
KillMode=control-group
UMask=0077
StandardOutput=null
StandardError=null
UNIT
sudo -n install -m 644 "$unit" /etc/systemd/system/` + t3Service + `
sudo -n systemctl daemon-reload
sudo -n systemctl start ` + t3Service + `
for n in $(seq 1 60); do
 if curl -fsS http://127.0.0.1:3774/.well-known/t3/environment >/dev/null 2>&1; then break; fi
 sleep 1
done
curl -fsS http://127.0.0.1:3774/.well-known/t3/environment >/dev/null
test -e ` + t3Cgroup + `/cgroup.kill
test -e ` + t3Home + `/userdata/server-runtime.json
`
	_, e := r.Client.Shell(t.Box, command)
	return e
}
func (i Instabox) InstallT3Guest(box, path string) error {
	if path == "" {
		if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
			return errors.New("set guest_binary to a Linux amd64 build of codex-remote (GOOS=linux GOARCH=amd64 go build)")
		}
		var e error
		path, e = os.Executable()
		if e != nil {
			return e
		}
	}
	path = expandHome(path)
	// Validate ELF architecture before a remote mutation, never copy a Mac binary.
	file, e := os.Open(path)
	if e != nil {
		return errors.New("guest binary unavailable")
	}
	var h [20]byte
	_, e = file.Read(h[:])
	file.Close()
	if e != nil || string(h[:4]) != "\x7fELF" || h[4] != 2 || h[5] != 1 || h[18] != 62 || h[19] != 0 {
		return errors.New("guest_binary must be a Linux amd64 executable")
	}
	local, err := os.Open(path)
	if err != nil {
		return errors.New("guest binary unavailable")
	}
	hash := sha256.New()
	_, err = io.Copy(hash, local)
	local.Close()
	if err != nil {
		return err
	}
	installed, err := i.Shell(box, "if test -x "+t3Helper+"; then sha256sum "+t3Helper+" | cut -d ' ' -f 1; fi")
	if err != nil {
		return err
	}
	if strings.TrimSpace(installed) == hex.EncodeToString(hash.Sum(nil)) {
		return nil
	}
	if _, e = i.Shell(box, "umask 077; mkdir -p /data/home/.local/bin"); e != nil {
		return e
	}
	if e = i.copy(path, box+":"+t3Helper+".pending"); e != nil {
		return e
	}
	_, e = i.Shell(box, "chmod 700 "+t3Helper+".pending; mv "+t3Helper+".pending "+t3Helper)
	return e
}

func t3GuestState(client Client, t Task, op string) (t3State, error) {
	var v t3State
	raw, err := client.Shell(t.Box, t3Helper+" t3-guest "+op+" "+quote(t.Conversation))
	if err != nil {
		return v, err
	}
	if json.Unmarshal([]byte(raw), &v) != nil || v.Thread != t.Conversation || v.Run == "" {
		return v, errors.New("invalid T3 lifecycle response; preserve lease")
	}
	return v, nil
}
func (r *Runner) quiesceT3(t Task) error {
	if t.Conversation == "" {
		return nil
	} // No T3 session exists during bootstrap/preparation.
	v, err := t3GuestState(r.Client, t, "quiesce")
	if err != nil {
		return err
	}
	if v.Busy || !v.Stopped {
		return errT3Busy
	}
	return nil
}
func (r *Runner) runT3(t Task, prompt, model string) (StartResult, error) {
	var result StartResult
	if t.Repo == "" {
		return result, errors.New("prepare the task repository before T3 dispatch")
	}
	if model == "" {
		_ = r.State.DB.QueryRow("SELECT model FROM t3_tasks WHERE ticket=?", t.Ticket).Scan(&model)
		if model == "" {
			return result, errors.New("first T3 run requires --model")
		}
	}
	var err error
	t, err = r.Activate(t.Ticket)
	if err != nil {
		return result, err
	}
	if _, err = r.Client.Shell(t.Box, officialEnvironmentCheck+"; codex login status"); err != nil {
		return result, err
	}
	if err = r.setupT3(t); err != nil {
		return result, err
	}
	if t.Conversation == "" {
		raw, err := r.Client.Shell(t.Box, t3Helper+" t3-guest project-exists")
		if err != nil {
			return result, err
		}
		var project struct {
			Exists bool `json:"exists"`
		}
		if json.Unmarshal([]byte(raw), &project) != nil {
			return result, errors.New("cannot inspect T3 project")
		}
		if !project.Exists {
			if _, err = r.Client.Shell(t.Box, t3CLI+" project add --title "+quote(t.Ticket)+" /data/home/workspace >/dev/null"); err != nil {
				return result, err
			}
		}
	}

	nonce := randomID()
	mode := r.Supervision
	if mode == "" {
		mode = "host"
	}
	tx, err := r.State.DB.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE tasks SET phase='starting-turn' WHERE ticket=?", t.Ticket); err != nil {
		return result, err
	}
	if _, err = tx.Exec("UPDATE t3_tasks SET model=?,nonce=? WHERE ticket=?", model, nonce, t.Ticket); err != nil {
		return result, err
	}
	if _, err = tx.Exec("INSERT INTO turns(ticket,conversation,job,mode,status) VALUES(?,?,'',?,'uncertain') ON CONFLICT(ticket) DO UPDATE SET conversation=excluded.conversation,mode=excluded.mode,status='uncertain',job='',note=''", t.Ticket, t.Conversation, mode); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}

	// Persist a private result on the guest before returning. A disconnected SSH
	// never licenses a retry: recover-turn reads this exact nonce's receipt.
	dir := t3Home + "/receipts"
	receipt := dir + "/" + nonce + ".json"
	cmd := t3CLI + " thread send --json " + quote(t.Conversation) + " -"
	if t.Conversation == "" {
		cmd = t3CLI + " thread start --json --title " + quote(t.Ticket) + " --provider codex --model " + quote(model) + " /data/home/workspace -"
	}
	script := "set -eu; umask 077; mkdir -p " + dir + "; " + cmd + " > " + receipt + "; cat " + receipt
	raw, err := r.Client.Shell(t.Box, "printf %s "+quote(prompt)+" | bash -lc "+quote(script))
	if err != nil {
		return result, err
	}
	return r.recordT3Launch(t, []byte(raw), mode)
}
func (r *Runner) recordT3Launch(t Task, raw []byte, mode string) (StartResult, error) {
	var v struct {
		Thread string `json:"threadId"`
		Run    string `json:"runId"`
	}
	var result StartResult
	if json.Unmarshal(raw, &v) != nil || v.Run == "" {
		return result, errors.New("T3 start outcome uncertain; use recover-turn, never dispatch again")
	}
	if v.Thread == "" {
		v.Thread = t.Conversation
	}
	if v.Thread == "" || (t.Conversation != "" && t.Conversation != v.Thread) {
		return result, errors.New("T3 thread mismatch; reconcile")
	}
	t.Conversation = v.Thread
	var show struct {
		Thread struct {
			ID       string `json:"threadId"`
			Link     string `json:"link"`
			Provider string `json:"providerInstanceId"`
		} `json:"thread"`
	}
	output, err := r.Client.Shell(t.Box, t3CLI+" thread show --messages 0 --json "+quote(v.Thread))
	if err != nil || json.Unmarshal([]byte(output), &show) != nil || show.Thread.Provider != "codex" || show.Thread.ID != v.Thread {
		return result, errors.New("cannot verify T3 thread; recover original launch")
	}
	link := ""
	if u, e := url.Parse(show.Thread.Link); e == nil && u.Path != "" {
		link = u.EscapedPath()
	}
	if link == "" {
		return result, errors.New("T3 thread link unavailable; recover original launch")
	}
	tx, err := r.State.DB.Begin()
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE t3_tasks SET link=? WHERE ticket=?", link, t.Ticket); err != nil {
		return result, err
	}
	if _, err = tx.Exec("UPDATE tasks SET phase='active',conversation=? WHERE ticket=?", v.Thread, t.Ticket); err != nil {
		return result, err
	}
	if _, err = tx.Exec("INSERT INTO turns(ticket,conversation,job,mode,status) VALUES(?,?,?,?,'active') ON CONFLICT(ticket) DO UPDATE SET conversation=excluded.conversation,job=excluded.job,mode=excluded.mode,status='active',note=''", t.Ticket, v.Thread, v.Run, mode); err != nil {
		return result, err
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}

	result = StartResult{ID: v.Thread, Job: v.Run}
	if r.Config.T3PublicURL != "" {
		result.URL = strings.TrimRight(r.Config.T3PublicURL, "/") + link
	}
	return result, err
}
func (r *Runner) recoverT3(t Task, conv string) (Task, error) {
	var nonce, mode string
	if err := r.State.DB.QueryRow("SELECT nonce FROM t3_tasks WHERE ticket=?", t.Ticket).Scan(&nonce); err != nil {
		return t, err
	}
	if validID(nonce) != nil {
		return t, errors.New("no durable T3 receipt nonce; inspect guest without dispatch")
	}
	if err := r.State.DB.QueryRow("SELECT mode FROM turns WHERE ticket=?", t.Ticket).Scan(&mode); err != nil {
		return t, err
	}
	raw, err := r.Client.Shell(t.Box, "cat "+t3Home+"/receipts/"+nonce+".json")
	if err != nil || !json.Valid([]byte(raw)) {
		if conv == "" || (t.Conversation != "" && t.Conversation != conv) {
			return t, errors.New("T3 receipt unavailable; inspect existing threads, then select the original --conversation without redispatching")
		}
		selected := t
		selected.Conversation = conv
		v, e := t3GuestState(r.Client, selected, "status")
		if e != nil {
			return t, e
		}
		// An explicit operator selection is required when the launch receipt was
		// lost before publication. Read the existing run; never create a new one.
		b, _ := json.Marshal(map[string]string{"threadId": conv, "runId": v.Run})
		raw = string(b)
	}

	var receipt struct {
		Thread string `json:"threadId"`
	}
	if json.Unmarshal([]byte(raw), &receipt) != nil || (conv != "" && receipt.Thread != "" && receipt.Thread != conv) {
		return t, errors.New("T3 receipt does not match selected thread")
	}
	_, err = r.recordT3Launch(t, []byte(raw), mode)
	if err != nil {
		return t, err
	}
	return r.State.Task(t.Ticket)
}

func (i Instabox) FollowT3(ctx context.Context, box, thread, after string) error {
	cmd := exec.CommandContext(ctx, "instabox", "ssh", box, "--", t3Helper+" t3-guest follow "+quote(thread)+" "+quote(after))
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		return errors.New("T3 lifecycle event follower ended; reconcile")
	}
	return nil
}

// Use SSH rather than exec: pairing output must not enter Instabox's retained
// exec history. Only the private local file and authenticated browser receive it.
func (i Instabox) PairT3(box, origin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "instabox", "ssh", box, "--", t3CLI+" auth pairing create --ttl 5m --base-url "+quote(origin)+" --json")
	configureProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	output := &boundedOutput{limit: 16 << 10}
	cmd.Stdout = output
	cmd.WaitDelay = 2 * time.Second
	if cmd.Run() != nil {
		return "", errors.New("private T3 pairing failed; credential output was not logged")
	}
	return string(output.data), nil
}

func t3Pair(r *Runner, ticket, path string) (any, error) {
	if !validT3Origin(r.Config.T3PublicURL) || path == "" {
		return nil, errors.New("set t3_public_url and --output-file to a private path")
	}
	t, err := r.State.Task(ticket)
	if err != nil {
		return nil, err
	}
	if t.Backend != "t3" || t.Phase != "active" {
		return nil, errors.New("pairing requires an active T3 task")
	}
	// Create the private destination first, never overwrite an existing pairing.
	f, err := os.OpenFile(expandHome(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, errors.New("cannot create private pairing file; choose an unused path")
	}
	defer f.Close()
	var raw string
	if t.Host == "local" {
		if !r.localControl().Online(t) {
			os.Remove(f.Name())
			return nil, errors.New("local T3 server is offline; start its service before pairing")
		}
		raw, err = r.localControl().Pair(t, r.Config.T3PublicURL)
	} else {
		if _, err = r.owned(t); err != nil {
			os.Remove(f.Name())
			return nil, err
		}
		pairer, ok := r.Client.(interface {
			PairT3(string, string) (string, error)
		})
		if !ok {
			os.Remove(f.Name())
			return nil, errors.New("client does not support private T3 pairing")
		}
		raw, err = pairer.PairT3(t.Box, r.Config.T3PublicURL)
	}
	if err != nil {
		os.Remove(f.Name())
		return nil, err
	}
	if _, err = f.WriteString(raw); err != nil {
		return nil, err
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	return map[string]string{"status": "pairing-saved", "file": filepath.Clean(expandHome(path)), "note": "One-time credential expires in 5 minutes. Open privately; never paste into chat or tickets."}, nil
}

const officialEnvironmentCheck = `set -e; set +u; test "${CODEX_HOME:-/data/home/.codex}" = /data/home/.codex; test "${CLAUDE_CONFIG_DIR:-/data/home/.claude}" = /data/home/.claude; for name in OPENAI_API_KEY CODEX_API_KEY CODEX_ACCESS_TOKEN OPENAI_BASE_URL ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL CLAUDE_CODE_OAUTH_TOKEN CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY; do test -z "${!name}" || exit 1; done`

func (r *Runner) startT3(t Task) error {
	_, err := r.Client.Shell(t.Box, officialEnvironmentCheck+"; if ! sudo -n systemctl is-active --quiet "+t3Service+"; then sudo -n systemctl reset-failed "+t3Service+"; fi; sudo -n systemctl start "+t3Service+"; for n in $(seq 1 30); do if curl -fsS http://127.0.0.1:3774/.well-known/t3/environment >/dev/null 2>&1; then exit 0; fi; sleep 1; done; exit 1")
	return err
}
func (r *Runner) armT3(t Task) error {
	var status string
	if err := r.State.DB.QueryRow("SELECT status FROM turns WHERE ticket=?", t.Ticket).Scan(&status); err != nil {
		return err
	}
	if status != "done" {
		return nil
	}
	v, err := t3GuestState(r.Client, t, "status")
	if err != nil {
		return err
	}
	_, err = r.State.DB.Exec("UPDATE turns SET status='awaiting-input',mode='host',job=?,note='' WHERE ticket=? AND status='done'", v.Run, t.Ticket)
	return err
}

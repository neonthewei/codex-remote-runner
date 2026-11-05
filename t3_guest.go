package main

// The guest guard is deliberately pinned to T3 0.1.6's durable schema. It never
// writes T3's database. Linux filesystem notifications wake the follower; a
// frozen systemd cgroup makes the final idle check atomic with respect to input.
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const t3Version = "0.1.6"
const t3Home = "/data/home/.codex-remote-t3"
const t3DB = t3Home + "/userdata/statev2.sqlite"
const t3Service = "codex-remote-t3.service"
const t3Cgroup = "/sys/fs/cgroup/system.slice/" + t3Service
const t3Helper = "/data/home/.local/bin/codex-remote-guest"
const t3Bin = "/data/home/.t3/runtime/versions/" + t3Version + "/t3"
const t3CLI = "T3CODE_HOME=" + t3Home + " " + t3Bin

var errT3Busy = errors.New("T3 has active, queued or pending work; preserve credentials and lease")

type t3State struct {
	Thread   string `json:"thread"`
	Run      string `json:"run"`
	Status   string `json:"status"`
	Busy     bool   `json:"busy"`
	Stopped  bool   `json:"stopped"`
	Sequence int64  `json:"sequence"`
}

func openT3DB(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	db, err := sql.Open("sqlite", u.String()+"?mode=ro&_pragma=busy_timeout(1500)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func inspectT3DB(db *sql.DB, thread string) (t3State, error) {
	v := t3State{Thread: thread, Busy: true}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return v, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow(`SELECT run_id,status FROM orchestration_v2_projection_runs WHERE thread_id=? ORDER BY ordinal DESC LIMIT 1`, thread).Scan(&v.Run, &v.Status); err != nil {
		return v, errors.New("T3 run not found; reconcile the original launch")
	}
	// Check the whole VM, including threads created by the human, subagents,
	// pending questions, queued input, finalization and scheduled background work.
	checks := []string{
		`SELECT count(*) FROM orchestration_v2_projection_runs WHERE status NOT IN ('completed','interrupted','failed','cancelled','rolled_back')`,
		`SELECT count(*) FROM orchestration_v2_projection_runtime_requests WHERE status NOT IN ('resolved','expired','cancelled')`,
		`SELECT count(*) FROM orchestration_v2_effect_outbox WHERE status NOT IN ('succeeded','cancelled')`,
		`SELECT count(*) FROM scheduled_tasks WHERE enabled<>0`,
		`SELECT count(*) FROM orchestration_v2_thread_launch_workflows WHERE status NOT IN ('completed','failed')`,
	}
	busy := false
	for _, query := range checks {
		var n int
		if err = tx.QueryRow(query).Scan(&n); err != nil {
			return v, errors.New("unsupported T3 state schema; preserve work")
		}
		busy = busy || n != 0
	}
	var applied int64
	if err = tx.QueryRow(`SELECT coalesce(max(sequence),0) FROM orchestration_events WHERE application_event_version=2 AND aggregate_kind='thread'`).Scan(&v.Sequence); err != nil {
		return v, err
	}
	if err = tx.QueryRow(`SELECT last_sequence FROM orchestration_v2_projection_metadata WHERE projection_name='thread-projections'`).Scan(&applied); err != nil {
		return v, err
	}
	v.Busy = busy || applied != v.Sequence
	return v, tx.Commit()
}

func guestSystemctl(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if exec.CommandContext(ctx, "sudo", append([]string{"-n", "systemctl"}, args...)...).Run() != nil {
		return errors.New("T3 service operation failed; preserve credentials and inspect guest service")
	}
	return nil
}
func verifyT3Version() error {
	raw, err := os.ReadFile(t3Home + "/userdata/server-runtime.json")
	var v struct {
		Version string `json:"serverVersion"`
	}
	if err != nil || json.Unmarshal(raw, &v) != nil || v.Version != t3Version {
		return errors.New("T3 runtime version mismatch; expected " + t3Version)
	}
	return nil
}

func guestQuiesce(thread string) (t3State, error) {
	var v t3State
	if err := verifyT3Version(); err != nil {
		return v, err
	}
	// An inactive service can be the result of a previous successful kill whose
	// response was lost. Recheck its durable state without restarting providers.
	frozen := false
	if _, err := os.Stat(t3Cgroup + "/cgroup.events"); err == nil {
		if err = guestSystemctl("freeze", t3Service); err != nil {
			return v, err
		}
		frozen = true
		defer func() {
			if frozen {
				_ = guestSystemctl("thaw", t3Service)
			}
		}()
		raw, err := os.ReadFile(t3Cgroup + "/cgroup.events")
		if err != nil || !strings.Contains(string(raw), "frozen 1") {
			return v, errors.New("T3 cgroup did not freeze; shutdown refused")
		}
	}
	if !frozen {
		if conn, e := net.DialTimeout("tcp", "127.0.0.1:3774", time.Second); e == nil {
			conn.Close()
			return v, errors.New("T3 listener exists outside managed cgroup; checkpoint refused")
		}
	}
	db, err := openT3DB(t3DB)
	if err != nil {
		return v, err
	}
	defer db.Close()
	v, err = inspectT3DB(db, thread)
	if err != nil || v.Busy {
		return v, err
	}
	if frozen {
		safe, e := idleT3Processes(t3Cgroup)
		if e != nil {
			return v, e
		}
		if !safe {
			v.Busy = true
			return v, nil
		}
	}
	if frozen {
		// cgroup.kill is atomic for the entire subtree. Do not thaw a checked idle
		// server to shut it down: its socket may already hold the next human prompt.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if exec.CommandContext(ctx, "sudo", "-n", "sh", "-c", "echo 1 > "+t3Cgroup+"/cgroup.kill").Run() != nil {
			return v, errors.New("cannot stop frozen T3 group; credentials retained")
		}
		// SIGKILL is now irrevocable for every member. Clear systemd's freezer
		// state before stopping the empty unit, or its next start is refused.
		if err = guestSystemctl("thaw", t3Service); err != nil {
			// The main process may have exited before thaw was handled. systemd
			// refuses thaw on a failed unit; reset-failed clears that stale state.
			if err = guestSystemctl("reset-failed", t3Service); err != nil {
				return v, err
			}
		}
		frozen = false
	}
	if err = guestSystemctl("stop", t3Service); err != nil {
		return v, err
	}
	if raw, err := os.ReadFile(t3Cgroup + "/cgroup.events"); err == nil && strings.Contains(string(raw), "populated 1") {
		return v, errors.New("T3 service still has processes; checkpoint refused")
	}
	v.Stopped = true
	return v, nil
}

// Do not kill a detached command or a human's terminal job merely because the
// agent run ended. Only known idle service/provider/MCP/desktop processes may
// remain. Unknown integrations fail closed until explicitly supported.
func idleT3Processes(group string) (bool, error) {
	raw, err := os.ReadFile(group + "/cgroup.procs")
	if err != nil {
		return false, err
	}
	for _, pid := range strings.Fields(string(raw)) {
		cmd, err := os.ReadFile("/proc/" + pid + "/cmdline")
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return false, err
		}
		args := strings.Split(strings.TrimRight(string(cmd), "\x00"), "\x00")
		if !idleT3Command(args) {
			return false, nil
		}
	}
	return true, nil
}
func idleT3Command(args []string) bool {
	if len(args) == 0 || args[0] == "" {
		return false
	}
	name := filepath.Base(args[0])
	switch name {
	case "t3":
		return len(args) > 1 && args[0] == t3Bin && args[1] == "serve"
	case "t3-resource-monitor", "codex-code-mode-host":
		return true
	case "codex":
		return len(args) == 2 && args[1] == "app-server"
	case "node":
		return len(args) >= 2 && ((args[1] == "/usr/local/bin/codex" && len(args) == 3 && args[2] == "app-server") || (args[1] == "/usr/local/bin/open-computer-use" && len(args) == 3 && args[2] == "mcp"))
	case "python3":
		command := strings.Join(args[1:], " ")
		return command == "-I /usr/local/bin/instabox-computer-use-proxy -- /usr/local/bin/open-computer-use mcp" || command == "/usr/local/bin/instabox-mcp mcp"
	case "open-computer-use":
		return len(args) == 2 && args[1] == "mcp"
	case "Xvnc", "xfce4-session", "ssh-agent", "xfwm4", "xfsettingsd", "xfce4-panel", "Thunar", "xfdesktop", "wrapper-2.0":
		return true
	}
	return false
}

func guestFollow(ctx context.Context, thread, after string) error {
	if err := verifyT3Version(); err != nil {
		return err
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer watcher.Close()
	// Watch the directory, since SQLite can replace/remove the WAL on restart.
	if err = watcher.Add(filepath.Dir(t3DB)); err != nil {
		return err
	}
	db, err := openT3DB(t3DB)
	if err != nil {
		return err
	}
	defer db.Close()
	check := func() (bool, error) {
		v, e := inspectT3DB(db, thread)
		return !v.Busy && (after == "" || v.Run != after), e
	}
	done, err := check()
	if err != nil || done {
		return err
	}
	var timer *time.Timer
	var timerC <-chan time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-watcher.Errors:
			if err == nil {
				err = errors.New("T3 event watch closed")
			}
			return err
		case event, ok := <-watcher.Events:
			if !ok {
				return errors.New("T3 event watch closed")
			}
			if !strings.HasPrefix(filepath.Base(event.Name), "statev2.sqlite") {
				continue
			}
			if timerC == nil {
				timer = time.NewTimer(250 * time.Millisecond)
				timerC = timer.C
			}
		case <-timerC:
			timerC = nil
			done, err = check()
			if err != nil || done {
				return err
			}
		}
	}
}

func runT3Guest(args []string) (any, error) {
	if runtime.GOOS != "linux" || len(args) < 1 {
		return nil, errors.New("T3 guest helper requires Linux")
	}
	if args[0] == "tunnel" && len(args) == 1 {
		c, err := net.DialTimeout("tcp", "127.0.0.1:3774", 10*time.Second)
		if err != nil {
			return nil, err
		}
		defer c.Close()
		if _, err = os.Stdout.Write([]byte{0}); err != nil {
			return nil, err
		}
		go func() {
			_, _ = io.Copy(c, os.Stdin)
			if tcp, ok := c.(*net.TCPConn); ok {
				_ = tcp.CloseWrite()
			}
		}()
		_, err = io.Copy(os.Stdout, c)
		return nil, err
	}
	if args[0] == "project-exists" && len(args) == 1 {
		db, err := openT3DB(t3DB)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		var n int
		if err = db.QueryRow("SELECT count(*) FROM projection_projects WHERE workspace_root='/data/home/workspace' AND deleted_at IS NULL").Scan(&n); err != nil {
			return nil, err
		}
		return map[string]bool{"exists": n == 1}, nil
	}
	if args[0] == "follow" && len(args) == 3 && len(args[1]) <= 256 && len(args[2]) <= 512 && !strings.ContainsAny(args[1]+args[2], "\x00\r\n") {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return nil, guestFollow(ctx, args[1], args[2])
	}
	if len(args) != 2 || len(args[1]) > 256 || strings.ContainsAny(args[1], "\x00\r\n") {
		return nil, errors.New("guest command requires an existing T3 thread")
	}
	switch args[0] {
	case "status":
		if err := verifyT3Version(); err != nil {
			return nil, err
		}
		db, err := openT3DB(t3DB)
		if err != nil {
			return nil, err
		}
		defer db.Close()
		return inspectT3DB(db, args[1])
	case "quiesce":
		return guestQuiesce(args[1])
	case "follow":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return nil, guestFollow(ctx, args[1], "")
	}
	return nil, fmt.Errorf("unknown T3 guest operation")
}

package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gofrs/flock"
	"html/template"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// This is only a private same-origin gateway. All coding UI, streaming,
// approvals, diffs and prompts are served unchanged by T3 in the task VM.
type t3UI struct {
	root, config, task, origin, csrf string
	password                         [32]byte
	proxy                            *httputil.ReverseProxy
}

func privatePassword(path string) ([32]byte, error) {
	var digest [32]byte
	info, err := os.Lstat(expandHome(path))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
		return digest, errors.New("provide a private regular password file (mode 0600)")
	}
	raw, err := os.ReadFile(expandHome(path))
	if err != nil {
		return digest, err
	}
	password := strings.TrimSpace(string(raw))
	if len(password) < 24 {
		return digest, errors.New("password requires at least 24 random characters")
	}
	return sha256.Sum256([]byte(password)), nil
}
func serveT3UI(root, config string, args []string) error {
	c, err := loadConfig(expandHome(config), os.LookupEnv)
	if err != nil {
		return err
	}
	f := flag.NewFlagSet("ui", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8788", "loopback listener")
	origin := f.String("public-url", c.T3PublicURL, "HTTPS origin reported by Tailscale Serve")
	pw := f.String("password-file", "", "private gateway password")
	reordered, err := reorder(args)
	if err != nil {
		return err
	}
	if err = f.Parse(reordered); err != nil {
		return err
	}
	id, err := requireArg(f.Args())
	if err != nil {
		return err
	}
	if !validT3Origin(*origin) {
		return errors.New("set t3_public_url to this gateway's private HTTPS origin")
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("ui requires a loopback listener behind private HTTPS")
	}
	digest, err := privatePassword(*pw)
	if err != nil {
		return err
	}
	s, err := openState(expandHome(root))
	if err != nil {
		return err
	}
	t, err := s.Task(id)
	root = s.Root
	s.Close()
	if err != nil {
		return err
	}
	if t.Backend != "t3" {
		return errors.New("this task uses the native backend; create a separate task with backend: t3")
	}
	ui := &t3UI{root: root, config: config, task: id, origin: strings.TrimRight(*origin, "/"), csrf: randomID(), password: digest}
	target, _ := url.Parse("http://127.0.0.1:3774")
	transport := &http.Transport{DialContext: ui.dial, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 8, IdleConnTimeout: 20 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
	ui.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			// T3 verifies same-origin requests. Preserve the public origin explicitly;
			// never trust a caller-supplied Forwarded host for routing or access.
			public, _ := url.Parse(ui.origin)
			pr.Out.Host = public.Host
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
			pr.Out.Header.Set("X-Forwarded-Host", public.Host)
			// Basic authentication is only for this gateway, not a model credential.
			pr.Out.Header.Del("Authorization")
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			http.Error(w, "T3 is reconnecting or checkpointing. Reload to resume the task.", 503)
		},
	}
	server := &http.Server{Addr: *listen, Handler: ui, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	fmt.Fprintf(os.Stderr, "T3 gateway for %s on http://%s; private HTTPS origin: %s\n", id, *listen, ui.origin)
	return server.ListenAndServe()
}
func (u *t3UI) taskState() (Task, error) {
	s, err := openObservationState(u.root)
	if err != nil {
		return Task{}, err
	}
	defer s.DB.Close()
	return s.Task(u.task)
}
func (u *t3UI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	user, pw, ok := r.BasicAuth()
	digest := sha256.Sum256([]byte(pw))
	if !ok || user != "operator" || subtle.ConstantTimeCompare(digest[:], u.password[:]) != 1 {
		w.Header().Set("WWW-Authenticate", `Basic realm="Instabox T3", charset="UTF-8"`)
		http.Error(w, "Authentication required", 401)
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != u.origin {
		http.Error(w, "Origin refused", 403)
		return
	}
	if r.URL.Path == "/_ib/connect" && r.Method == "GET" {
		task, err := u.taskState()
		if err != nil {
			http.Error(w, "Task unavailable", 503)
			return
		}
		if task.Phase == "paused" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
			_ = t3ResumePage.Execute(w, struct{ Task, CSRF, Next string }{u.task, u.csrf, "/_ib/connect"})
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		_ = t3ConnectPage.Execute(w, struct{ CSRF string }{u.csrf})
		return
	}
	if r.URL.Path == "/_ib/resume" || r.URL.Path == "/_ib/connect" {
		if r.Method != "POST" {
			w.Header().Set("Allow", "POST")
			http.Error(w, "Use the resume button", 405)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if r.Header.Get("Origin") != u.origin || r.ParseForm() != nil || subtle.ConstantTimeCompare([]byte(r.PostForm.Get("csrf")), []byte(u.csrf)) != 1 {
			http.Error(w, "Invalid resume request", 403)
			return
		}
		if r.URL.Path == "/_ib/connect" {
			dir, err := os.MkdirTemp(u.root, ".pair-")
			if err != nil {
				http.Error(w, "Pairing unavailable", 503)
				return
			}
			defer os.RemoveAll(dir)
			file := filepath.Join(dir, "pair.json")
			argv := []string{"--state", u.root}
			if u.config != "" {
				argv = append(argv, "--config", u.config)
			}
			if _, err = execute(append(argv, "pair", u.task, "--output-file", file)); err != nil {
				http.Error(w, "T3 pairing unavailable. Resume the task first or inspect controller status.", 503)
				return
			}
			raw, err := readPrivateFile(file, 16<<10)
			var pair struct {
				Credential string `json:"credential"`
			}
			if err != nil || json.Unmarshal(raw, &pair) != nil || pair.Credential == "" {
				http.Error(w, "Pairing unavailable", 503)
				return
			}
			// Fragment credentials are consumed by T3 in this authenticated browser;
			// they never go in a task link, request log, bot response or issue.
			http.Redirect(w, r, "/pair#token="+url.QueryEscape(pair.Credential), http.StatusSeeOther)
			return
		}
		argv := []string{"--state", u.root}
		if u.config != "" {
			argv = append(argv, "--config", u.config)
		}
		if _, err := execute(append(argv, "activate", u.task)); err != nil {
			http.Error(w, "Cannot resume. Inspect the task on the controller; its work and lease are preserved.", 503)
			return
		}
		next := r.PostForm.Get("next")
		if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsAny(next, "\\\r\n") {
			next = "/"
		}
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	t, err := u.taskState()
	if err != nil {
		http.Error(w, "Task state unavailable", 503)
		return
	}
	if t.Phase != "active" {
		if r.Method != "GET" || r.Header.Get("Upgrade") != "" {
			http.Error(w, "Task paused or checkpointing; reload to resume", 503)
			return
		}
		if t.Phase != "paused" {
			http.Error(w, "Task is checkpointing or needs recovery. Reload shortly; use controller status for errors.", 503)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
		_ = t3ResumePage.Execute(w, struct{ Task, CSRF, Next string }{u.task, u.csrf, r.URL.RequestURI()})
		return
	}
	u.proxy.ServeHTTP(w, r)
}

// Dial under a task gateway gate, and wait for the guest handshake before
// releasing it. A stale browser read must not start an SSH connection after
// checkpoint has paused the VM. No reconnecting/background port-forward exists.
func (u *t3UI) dial(ctx context.Context, network, address string) (net.Conn, error) {
	gate := flock.New(filepath.Join(u.root, "t3-"+u.task+".lock"))
	gateCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ok, err := gate.TryRLockContext(gateCtx, 100*time.Millisecond)
	if err != nil || !ok {
		return nil, errors.New("task is checkpointing")
	}
	defer gate.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s, err := openObservationState(u.root)
	if err != nil {
		return nil, err
	}
	defer s.DB.Close()
	t, err := s.Task(u.task)
	if err != nil {
		return nil, err
	}
	if t.Phase != "active" {
		return nil, errors.New("task is not active")
	}
	b, err := (&Runner{State: s, Client: Instabox{Timeout: 15 * time.Second}}).owned(t)
	if err != nil || b.State != "running" {
		return nil, errors.New("VM is not running; resume explicitly")
	}
	// A request context does not own a pooled connection. Its lifetime belongs
	// to Transport; cancellation below only bounds establishing the SSH stream.
	cmd := exec.Command("instabox", "ssh", t.Box, "--", t3Helper+" t3-guest tunnel")
	configureProcessGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, err
	}
	conn := &t3SSHConn{cmd: cmd, in: stdin, out: stdout}
	ready := make(chan error, 1)
	go func() {
		var b [1]byte
		_, e := io.ReadFull(stdout, b[:])
		if e == nil && b[0] != 0 {
			e = errors.New("invalid guest tunnel handshake")
		}
		ready <- e
	}()
	timer := time.NewTimer(25 * time.Second)
	defer timer.Stop()
	select {
	case err = <-ready:
		if err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	case <-ctx.Done():
		conn.Close()
		return nil, ctx.Err()
	case <-timer.C:
		conn.Close()
		return nil, errors.New("T3 private tunnel unavailable")
	}
}

type t3SSHConn struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (c *t3SSHConn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *t3SSHConn) Write(p []byte) (int, error) { return c.in.Write(p) }
func (c *t3SSHConn) Close() error {
	c.once.Do(func() {
		c.in.Close()
		c.out.Close()
		if c.cmd.Process != nil {
			_ = killProcessGroup(c.cmd)
		}
		_ = c.cmd.Wait()
	})
	return nil
}
func (c *t3SSHConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }
func (c *t3SSHConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 3774}
}
func (c *t3SSHConn) SetDeadline(time.Time) error      { return nil }
func (c *t3SSHConn) SetReadDeadline(time.Time) error  { return nil }
func (c *t3SSHConn) SetWriteDeadline(time.Time) error { return nil }

var t3ResumePage = template.Must(template.New("resume").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Resume T3 Code</title><style>:root{color-scheme:dark;font:16px/1.5 system-ui;background:#111113;color:#eee}body{max-width:30rem;margin:18vh auto;padding:24px}p{color:#aaa}button{font:inherit;border:0;border-radius:8px;background:#eee;color:#151515;padding:12px 20px;cursor:pointer}</style><h1>Task paused</h1><p>{{.Task}} is saved. Resume its VM to open the same conversation in T3 Code.</p><form method="post" action="/_ib/resume"><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="next" value="{{.Next}}"><button>Resume in T3 Code</button></form></html>`))

var t3ConnectPage = template.Must(template.New("connect").Parse(`<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Connect T3 Code</title><style>:root{color-scheme:dark;font:16px/1.5 system-ui;background:#111113;color:#eee}body{max-width:30rem;margin:18vh auto;padding:24px}p{color:#aaa}button{font:inherit;border:0;border-radius:8px;background:#eee;color:#151515;padding:12px 20px;cursor:pointer}</style><h1>Connect T3 Code</h1><p>Authorize this browser to view and continue this task. Your model login stays on the controller and task VM.</p><form method="post" action="/_ib/connect"><input type="hidden" name="csrf" value="{{.CSRF}}"><button>Connect this browser</button></form></html>`))

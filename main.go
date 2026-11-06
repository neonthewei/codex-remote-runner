package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const Version = "0.1.0"

func help() {
	fmt.Println(`codex-remote [--config PATH] [--state PATH] COMMAND

Commands:
  supervise                       Checkpoint completed host-managed turns
  watch ID                        Wait for one turn and checkpoint it
  linear --settings PATH          Optional Linear Agent Session webhook and worker
  serve --password-file PATH      Read-only dashboard on 127.0.0.1:8787
  ui ID --password-file PATH      T3 UI gateway on 127.0.0.1:8788 (private HTTPS)
  pair ID --output-file PATH      Save a short-lived T3 pairing link privately
  version                         Print controller version
  init-key                        Generate a private headless-store key (never overwrite)
  bootstrap                       Create dedicated official Codex login VM
  check-host                      Verify the configured remote Windows worker
  create --task ID                Create one task VM (configurable user/org/size)
  create-app-task --task ID --thread-id ID --worktree PATH
                                  Register a task created inside its remote Codex Project
  serve-task ID                   Run one worktree task's T3 server in the foreground
  stop-task ID                    Stop one managed Windows task's T3 server
  serve-web ID --mode production|preview
                                  Run the task's public Cloudflare web service
  web-status ID [--path /journey] Verify public HTTPS root and journey
  web-logs ID [--tail 200]        Read bounded dev server and tunnel logs
  web-inspect ID --path / --output-file PATH
                                  Capture screenshot and browser failures
  stop-web ID                     Stop the managed public web service
  prepare-preview ID              Explicitly prepare a Supabase preview
  serve-local ID                  Compatibility alias for serve-task
  handoff ID --to app|t3          Transfer the local task's single-writer owner
  archive-task ID                 Archive the task's Codex App thread
  unarchive-task ID               Restore the task's archived Codex App thread
  pin-task ID                     Move the Codex App thread into Pinned
  unpin-task ID                   Remove the Codex App thread from Pinned
  list-projects ID                List Codex App projects visible on the task host
  project-task ID --project-id ID Assign the Codex App thread to a project
  project-task ID --clear         Remove the Codex App project assignment
  rename-task ID --title TEXT     Rename the Codex App thread
  adopt-thread ID --thread-id ID  Bind an idle native App thread in this worktree
  create-app-thread ID --title TEXT [--project-id ID]
                                  Create a native App thread for collaboration
  app-run ID --prompt-file PATH   Continue an adopted native App thread
  prepare ID                      Clone configured repository and create task branch
  run ID --prompt-file PATH       Start or resume configured coding agent
  status ID                       Read task and VM status
  link-pr ID --url HTTPS_URL       Record a GitHub PR for the dashboard
  activate ID                     Resume VM and restore latest Codex auth
  checkpoint ID                   Save auth, pause VM, release profile lease
  reconcile ID                    Reconcile uncertain VM creation
  recover-prepare ID              Verify a partially prepared checkout
  recover-turn ID --conversation ID

Configuration: --config YAML, then CODEX_REMOTE_* environment, then command flags.
Task IDs work without an issue tracker.
No token values belong in YAML, environment overrides or task prompts.`)
}

func expandHome(path string) string {
	if path == "~" {
		h, e := os.UserHomeDir()
		if e == nil {
			return h
		}
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		h, e := os.UserHomeDir()
		if e == nil {
			return filepath.Join(h, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}
func reorder(args []string) ([]string, error) {
	flags, pos := []string{}, []string{}
	for n := 0; n < len(args); n++ {
		s := args[n]
		if strings.HasPrefix(s, "-") {
			flags = append(flags, s)
			if !strings.Contains(s, "=") && s != "--help" && s != "-h" {
				n++
				if n >= len(args) {
					return nil, errors.New("flag requires a value")
				}
				flags = append(flags, args[n])
			}
		} else {
			pos = append(pos, s)
		}
	}
	return append(flags, pos...), nil
}
func execute(args []string) (any, error) {
	if len(args) > 0 && args[0] == "t3-guest" {
		return runT3Guest(args[1:])
	}
	if len(args) > 0 && args[0] == "worker" {
		return runWindowsWorker(os.Stdin, args[1:])
	}
	if len(args) == 1 && (args[0] == "version" || args[0] == "--version") {
		return map[string]string{"version": Version}, nil
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		help()
		return nil, nil
	}
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".local", "state", "codex-remote-runner")
	configPath := os.Getenv("CODEX_REMOTE_CONFIG")
	if v := os.Getenv("CODEX_REMOTE_STATE"); v != "" {
		root = v
	}
	for len(args) > 0 && strings.HasPrefix(args[0], "--") {
		name, val, inline := strings.Cut(args[0], "=")
		if name != "--state" && name != "--config" {
			return nil, errors.New("global options must be --config or --state")
		}
		args = args[1:]
		if !inline {
			if len(args) == 0 {
				return nil, errors.New("missing global option value")
			}
			val = args[0]
			args = args[1:]
		}
		if name == "--state" {
			root = val
		} else {
			configPath = val
		}
	}
	if len(args) == 0 {
		return nil, errors.New("missing command")
	}
	action := args[0]
	if action == "supervise" || action == "watch" {
		return nil, runSupervisor(root, configPath, action, args[1:])
	}
	if action == "linear" {
		return nil, runLinear(root, configPath, args[1:])
	}
	if action == "serve" {
		return nil, serveDashboard(root, args[1:])
	}
	if action == "ui" {
		return nil, serveT3UI(root, configPath, args[1:])
	}
	c, e := loadConfig(expandHome(configPath), os.LookupEnv)
	if e != nil {
		return nil, e
	}
	f := flag.NewFlagSet(action, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	user := f.String("user", c.User, "")
	org := f.String("org", c.Org, "")
	provider := f.String("provider", c.Provider, "")
	engine := f.String("backend", c.Backend, "")
	host := f.String("host", c.Host, "")
	outputFile := f.String("output-file", "", "")
	repo := f.String("repo", c.Repo, "")
	base := f.String("base", c.Base, "")
	snapshot := f.String("snapshot", c.Snapshot, "")
	cpu := f.Int("vcpus", c.VCPUs, "")
	memory := f.Int("mem-mib", c.Memory, "")
	taskFlag := f.String("task", "", "")
	prompt := f.String("prompt-file", "", "")
	cwd := f.String("cwd", "/data/home", "")
	model := f.String("model", "", "")
	conv := f.String("conversation", "", "")
	prURL := f.String("url", "", "")
	supervision := f.String("supervision", "host", "")
	owner := f.String("to", "", "")
	projectID := f.String("project-id", "", "")
	clearProject := f.Bool("clear", false, "")
	threadID := f.String("thread-id", "", "")
	worktree := f.String("worktree", "", "")
	fullAccess := f.Bool("full-access", false, "")
	webMode := f.String("mode", "production", "")
	webPath := f.String("path", "/", "")
	webTail := f.Int("tail", 200, "")
	title := f.String("title", "", "")
	backend := f.String("credential-backend", c.CredentialBackend, "")
	keyFile := f.String("credential-key-file", c.CredentialKeyFile, "")
	reordered, e := reorder(args[1:])
	if e != nil {
		return nil, e
	}
	if e = f.Parse(reordered); e != nil {
		if e == flag.ErrHelp {
			help()
			return nil, nil
		}
		return nil, errors.New("invalid command arguments")
	}
	c.User = *user
	c.Org = *org
	c.Provider = *provider
	c.Backend = *engine
	c.Host = *host
	c.Repo = *repo
	c.Base = *base
	c.Snapshot = *snapshot
	c.VCPUs = *cpu
	c.Memory = *memory
	c.CredentialBackend = *backend
	c.CredentialKeyFile = *keyFile
	if e = c.Validate(); e != nil {
		return nil, e
	}
	if action == "check-host" {
		if len(f.Args()) != 0 || c.Host != "windows" {
			return nil, errors.New("check-host requires windows configuration and no positional arguments")
		}
		return (&windowsHost{config: c}).Health()
	}
	if action == "init-key" {
		if len(f.Args()) != 0 || c.CredentialBackend != "encrypted-file" {
			return nil, errors.New("init-key requires encrypted-file configuration and no positional arguments")
		}
		if e := initCredentialKey(expandHome(c.CredentialKeyFile)); e != nil {
			return nil, e
		}
		return map[string]string{"status": "key-created"}, nil
	}
	valid := map[string]bool{"pair": true, "link-pr": true, "create": true, "create-app-task": true, "bootstrap": true, "status": true, "prepare": true, "run": true, "activate": true, "checkpoint": true, "reconcile": true, "recover-prepare": true, "recover-turn": true, "serve-task": true, "stop-task": true, "serve-local": true, "handoff": true, "archive-task": true, "unarchive-task": true, "pin-task": true, "unpin-task": true, "list-projects": true, "project-task": true, "rename-task": true, "adopt-thread": true, "create-app-thread": true, "app-run": true, "serve-web": true, "web-status": true, "web-logs": true, "web-inspect": true, "stop-web": true, "prepare-preview": true}
	if !valid[action] {
		return nil, errors.New("unknown command")
	}
	ticket := ""
	if action != "create" && action != "create-app-task" && action != "bootstrap" {
		ticket, e = requireArg(f.Args())
		if e != nil {
			return nil, e
		}
	} else if len(f.Args()) > 0 {
		return nil, errors.New("unexpected positional arguments")
	}
	if action == "serve-task" || action == "serve-local" {
		s, err := openState(expandHome(root))
		if err != nil {
			return nil, err
		}
		task, err := s.Task(ticket)
		s.Close()
		if err != nil {
			return nil, err
		}
		if !isWorktreeHost(task.Host) {
			return nil, errors.New("serve-task requires a worktree task")
		}
		if task.Host == "windows" {
			return nil, (&windowsHost{config: c}).Serve(task)
		}
		return nil, (&localHost{config: c}).Serve(task)
	}
	if action == "serve-web" {
		if *webMode != "production" && *webMode != "preview" {
			return nil, errors.New("serve-web mode must be production or preview")
		}
		s, err := openState(expandHome(root))
		if err != nil {
			return nil, err
		}
		task, err := s.Task(ticket)
		s.Close()
		if err != nil {
			return nil, err
		}
		if task.Host != "windows" || task.Phase != "active" {
			return nil, errors.New("serve-web requires an active Windows worktree task")
		}
		return nil, (&windowsHost{config: c}).ServeWeb(task, *webMode)
	}
	s, e := openState(expandHome(root))
	if e != nil {
		return nil, e
	}
	defer s.Close()
	var secrets Secrets = &OSSecrets{service: "codex-remote/" + s.Namespace}
	if c.CredentialBackend == "encrypted-file" {
		secrets = &EncryptedSecrets{KeyFile: expandHome(c.CredentialKeyFile), Dir: filepath.Join(s.Root, "credentials"), Namespace: s.Namespace}
	}
	if *supervision != "host" && *supervision != "external" {
		return nil, errors.New("supervision must be host or external")
	}
	r := Runner{State: s, Client: Instabox{}, Secrets: secrets, Supervision: *supervision, Config: c}
	switch action {
	case "pair":
		return t3Pair(&r, ticket, *outputFile)
	case "link-pr":
		t, err := s.Task(ticket)
		if err != nil {
			return nil, err
		}
		if !validPRURL(*prURL, t.Repo) {
			return nil, errors.New("provide a GitHub pull request URL for the prepared repository")
		}
		t.PR = *prURL
		if err = s.Save(t); err != nil {
			return nil, err
		}
		return viewTask(t), nil
	case "create":
		return r.Create(*taskFlag, c, false)
	case "create-app-task":
		return r.CreateManagedAppTask(*taskFlag, c, *threadID, *worktree)
	case "bootstrap":
		return r.Bootstrap(c)
	case "status":
		return r.Status(ticket)
	case "stop-task":
		return map[string]string{"status": "stopped"}, r.StopTask(ticket)
	case "prepare-preview":
		return r.PreparePreview(ticket)
	case "web-status":
		return r.WebStatus(ticket, *webPath)
	case "web-logs":
		return r.WebLogs(ticket, *webTail)
	case "web-inspect":
		return r.WebInspect(ticket, *webPath, *outputFile)
	case "stop-web":
		return map[string]string{"status": "stopped"}, r.StopWeb(ticket)
	case "handoff":
		return r.HandoffLocal(ticket, *owner)
	case "archive-task", "unarchive-task", "pin-task", "unpin-task", "list-projects":
		return r.ManageCodexAppTask(ticket, action, "")
	case "project-task":
		if *clearProject == (*projectID != "") {
			return nil, errors.New("project-task requires exactly one of --project-id or --clear")
		}
		if *projectID != "" {
			if err := validID(*projectID); err != nil {
				return nil, errors.New("invalid Codex App project ID")
			}
		}
		return r.ManageCodexAppTask(ticket, action, *projectID)
	case "rename-task":
		return r.ManageCodexAppTask(ticket, action, *title)
	case "adopt-thread":
		if strings.TrimSpace(*threadID) == "" {
			return nil, errors.New("adopt-thread requires --thread-id")
		}
		return r.AdoptCodexAppThread(ticket, *threadID)
	case "create-app-thread":
		if *projectID != "" {
			if err := validID(*projectID); err != nil {
				return nil, errors.New("invalid Codex App project ID")
			}
		}
		return r.CreateCodexAppThread(ticket, *title, *projectID)
	case "app-run":
		if *prompt == "" {
			return nil, errors.New("provide a private task-text file using --prompt-file")
		}
		raw, readErr := os.ReadFile(expandHome(*prompt))
		if readErr != nil {
			return nil, errors.New("cannot read task-text file")
		}
		if len(raw) == 0 || len(raw) > 256*1024 {
			return nil, errors.New("task text must contain 1 to 262144 bytes")
		}
		return r.RunCodexAppThread(ticket, string(raw), *fullAccess)
	case "activate":
		return r.Activate(ticket)
	case "checkpoint":
		return r.Checkpoint(ticket)
	case "reconcile":
		return r.Reconcile(ticket)
	case "prepare":
		return r.Prepare(ticket, c.Repo, c.Base)
	case "recover-prepare":
		return r.RecoverPrepare(ticket)
	case "recover-turn":
		return r.RecoverTurn(ticket, *conv)
	case "run":
		task, taskErr := s.Task(ticket)
		if taskErr != nil {
			return nil, taskErr
		}
		if !isWorktreeHost(task.Host) && *supervision == "host" && !supervisorPresent(s.Root) {
			return nil, errors.New("start codex-remote supervise as a persistent host service before run, or explicitly use --supervision external with your own watcher")
		}
		if *prompt == "" {
			return nil, errors.New("provide a private task-text file using --prompt-file")
		}
		raw, e := os.ReadFile(expandHome(*prompt))
		if e != nil {
			return nil, errors.New("cannot read task-text file")
		}
		return r.Run(ticket, string(raw), *cwd, *model)
	}
	return nil, errors.New("unknown command")
}
func main() {
	v, e := execute(os.Args[1:])
	if e != nil {
		raw, _ := json.Marshal(map[string]string{"error": e.Error()})
		fmt.Fprintln(os.Stderr, string(raw))
		os.Exit(1)
	}
	if v != nil {
		raw, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			fmt.Fprintln(os.Stderr, `{"error":"cannot encode result"}`)
			os.Exit(1)
		}
		fmt.Println(string(raw))
	}
}

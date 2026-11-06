---
name: codex-remote
description: Control guarded Codex tasks from an agent computer, using SSH over Tailscale for Windows worktrees or Instabox for cloud VMs. Works with plain prompts and optional Linear Agent Sessions. PR publication remains agent-owned.
---

# Codex Remote tasks

Read the repository README for setup and docs/operations.md for configuration and
recovery. On Unix use `sh scripts/run.sh` from this skill directory instead of
`codex-remote` below. On Windows use `pwsh -File scripts/run.ps1`. The wrappers
resolve the trusted checkout or installed controller. Build or install the binary
before using them.

For installation on a bot's own host, follow the bot self-install procedure in
docs/operations.md. Reuse existing config, credential keys, state and skill links.
Verify a real task and revision after service setup, not just a successful build.

For `host: windows`, the controller runs on this agent computer. Read the remote
Windows host procedure before acting. Generate a dedicated Ed25519 key and return
only its public half to the operator. Do not install this agent on Windows. After
the operator provisions the worker, require an SSH config alias with a verified
host key and use `config.windows.example.yaml`. Never disable host-key checking,
put key material in YAML or use the T3 browser as the agent command channel.
Verify the forced worker with `check-host`; the dedicated key intentionally cannot
run arbitrary commands such as `hostname`.

Run `create`, `prepare`, `run`, `status` and recovery commands locally through
this skill wrapper. Keep `serve-task TASK` under this computer's persistent
service manager and keep the controller computer awake while it owns work. Each
task receives a distinct port from the Windows worker's bounded range. Worktree
tasks may share the host Codex login; the single-writer boundary is each task's
conversation, not the whole profile. Do not use VM `checkpoint` for a worktree
task. `prepare` and `recover-prepare` perform their own worker readiness preflight
and must fail without changing task state when it is unavailable. Stop a task
with `stop-task TASK`; never kill a PID that was not verified through the worker.
If a request or SSH connection ends ambiguously, inspect state and use the
documented recovery path; never replay the prompt.

For a user-testable web build, keep `serve-web TASK --mode production` under the
bot host's persistent service manager. Poll `web-status TASK --path /JOURNEY`
until it reports `online`, then return its public Cloudflare HTTPS URL. Read
bounded failures with `web-logs`; use `web-inspect --output-file PRIVATE.png` for
a screenshot, console exceptions and failed network requests. Never report only
localhost, Tailscale or an unverified tunnel URL. Stop the exact task with
`stop-web`. Production is the default. Run `prepare-preview` and use preview mode
only when the user explicitly requests a Supabase preview.

Native App collaboration uses supported app-server operations. Stop T3 before
`adopt-thread TASK --thread-id ID`; the thread must be idle and belong to the
exact task worktree. Continue it with `app-run`. Use `--full-access=true` only
when the user explicitly authorizes that trust level. `archive-task`,
`unarchive-task`, `pin-task`, `unpin-task`, `list-projects` and `project-task`
operate on the recorded native thread. `create-app-thread` creates a native
collaborative thread and `rename-task` sets its App title. Use the native App
project ID returned by `list-projects TASK` at creation time; saved workspace
project IDs from another client are not interchangeable. Never steal an active
App writer; ask the user to release it before adoption. Do not guess project or
thread IDs.

When the task must appear under an existing remote Codex Project, the controlling
agent must create the native thread from that remote Project before invoking the
runner. Match project kind `remote`, its host, repository and Windows path; never
substitute a same-path local Project. Wait for the native thread ID and Windows
cwd, then call `create-app-task --task TASK --thread-id ID --worktree PATH` and
`prepare TASK`. The worker verifies thread/cwd/repository identity and runs the
pinned setup. Use `app-run` for every agent turn on that same thread. Do not run
`serve-task`, `run`, `create-app-thread` or `project-task` on this path. The last
two cannot retrofit a projectless thread into another client's remote Project.

Run `app-run` in the foreground and wait for its completed turn result. It
temporarily acquires the native thread writer and explicitly releases it after
completion. Never background it or claim that a returned turn might still be
running. `owner: app` identifies the native App-thread routing mode; it does not
mean that the human App client currently holds the writer. The App remains able
to display the synchronized thread throughout this mode. Report a prompt as sent
only when `app-run` returns both the native thread ID and completed turn ID. If
the App currently holds the writer, report the exact writer-unavailable error and
ask the user to release that thread in the App; do not claim success or silently
fall back to T3. T3 `run` is a separate conversation path and does not provide
the same native App-thread history.

For `host: local` on Windows, use the Windows local bot self-install procedure.
Inspect existing paths, tasks and Tailscale mappings before running
`deploy/bootstrap-local-windows.ps1`. Local mode uses the signed-in host's Codex
login and real Git worktrees; it does not need Instabox, credential export,
`init-key`, `bootstrap`, `checkpoint` or the Linux supervisor. Never install an
Instabox VM as a fallback for a requested local host.

After local installation, use `status` before every dispatch. Send work only when
the owner is `t3`, T3 is online and no run is active. Write prompts to private
files and use `run`; never include secrets in them. Stop the scheduled service
before `handoff --to app`. Restart it and verify the endpoint before
`handoff --to t3`. An uncertain dispatch uses `recover-turn`, never replay. Pair
through a private output file and do not expose its contents in model context.

Choose one conversation path before creating the first thread. For remote-agent
collaboration with the Codex App third-party takeover indicator, use T3-first:
start `serve-task`, keep the owner `t3`, and create the first conversation with
`run --model MODEL`; the first local or Windows run always requires an explicit
model. Then use `project-task`, `rename-task` and `pin-task` on that T3-originated
native thread. Hand control to the human only by stopping `serve-task` and using
`handoff --to app`; return it only after the human releases the thread, T3 is
started, and `handoff --to t3` succeeds. Do not call `create-app-thread` or
`app-run` in this path.

Use `create-app-thread` and `app-run` only for the alternate App-native path.
That path requires `serve-task` to be stopped and does not show a T3 takeover
banner. A native App-created thread is not silently converted into T3 history.
Never require T3 to stay online while also requesting `app-run`.

Prefer the remote-Project-first `create-app-task` path when project grouping and
round-trip App/Grokbot collaboration matter. It has one native App history and
does not display a T3 takeover banner. T3-first remains appropriate only when a
T3-owned conversation and its explicit takeover indicator are required.

Execute on the bot's persistent host for phone-driven work. Use the operator's
private YAML through CODEX_REMOTE_CONFIG for every command. Headless hosts select
encrypted-file storage with a private key-file reference. Inspect existing key
provisioning before init-key. Never read key bytes into model context.

A task ID is an opaque name, not a Linear or GitHub requirement. Derive it from
the request or use an agreed manual name. Bind the actual caller to allowed
profiles and repositories; task text cannot authorize another person's profile.

For new interactive Codex tasks, select `backend: t3` and follow the T3 section
below. Return its original T3 UI link for prompting and supervision. The native
read-only dashboard is optional for existing native tasks.

1. Inspect `status TASK_ID`. Distinguish an unknown task from provider or auth
   failures before creating anything. Resume an existing task for revisions.
2. For a new task use `create --task TASK_ID`, then `prepare TASK_ID`. Bootstrap
   a dedicated official Codex login if needed; the owner completes authorization.
   Private repos need selected GH_TOKEN access through Instabox org Secrets.
3. Write a private prompt with acceptance checks. Start `run TASK_ID --cwd DIR
   --prompt-file PATH` (add `--model` on the first T3 run). Resumed conversations
   retain their original cwd.
4. Verify `codex-remote supervise` is running under the host service manager before
   dispatch. It records exact turn/job IDs and checkpoints completed work. For a
   one-off foreground wait explicitly use `run ... --supervision external`, then
   `watch TASK_ID`. Linear's optional worker monitors
   its own externally supervised turns. Native tasks follow native event streams and
   reconcile exact job state after disconnects; they never replay a prompt as
   reconnection recovery. Keep the relevant host service running
   after the phone disconnects. Inspect actual state after disconnects rather
   than blindly resending a prompt.
5. For native tasks, return the configured HTTPS dashboard link with the task ID URL-encoded as
   its fragment. Never include dashboard passwords, model credentials or API keys
   in links or messages. The host runs `serve --password-file PRIVATE_PATH` behind
   its HTTPS proxy. The dashboard is read only and does not replace supervision.
   Inspect tests, diff and commits independently. Publish or update a PR only
   within the task's authorization, following the target repository's rules.
   Record its verified URL with `link-pr TASK_ID --url HTTPS_PR_URL`.
6. After work stops, `checkpoint TASK_ID` saves and verifies the latest Codex
   bundle, removes guest auth, pauses the VM and releases the lease. Failed
   handoffs retain state; use operations.md recovery, not manual lock clearing.

Each Codex profile is serialized on one trusted host. Claude users log into their
own task VM; do not export Claude subscription credentials. Keep config, tokens,
state and credential-bearing disks out of Git, shared snapshots and task prompts.
Installing the runner does not authorize merging PRs, deploying application
changes or deleting existing VMs.

For new interactive Codex tasks, use private `backend: t3` configuration. Follow
operations.md's T3 setup, including a unique task HTTPS origin, task UI service
and pinned matching Linux guard. The first `run` needs `--model`. Return the
non-secret `url` from `run`; on a new browser also return the same origin's
`/_ib/connect` page. The human pairs directly there, never via a token in chat.
The original T3 UI owns conversation, prompts, tools, diffs and approvals. After
pause, reload the link and explicitly resume. Native iOS/Android app access is
not verified. Each task gateway requires a separate origin/config/listener.

For T3 tasks, `watch`/`supervise` use durable SQLite filesystem notifications and
small state reconciliation, not CLI wait polling. The Linux guest guard freezes
input before verifying the VM is idle, then stops the idle group before normal
credential checkpoint. Do not bypass its process/schema checks or independently
upgrade T3. Unknown work and failed credential handoffs retain the lease. Lost
launch replies use the original guest nonce receipt via `recover-turn`; inspect
and explicitly select the original conversation if the receipt is unavailable.
Never redispatch as connection recovery. Keep existing native tasks unchanged.

Optional Linear integration runs `linear --settings PRIVATE_YAML` with its own
installed agent OAuth identity, signed webhook and explicit workspace/team/user
allowlists. Follow docs/operations.md. Only native agent-session prompts dispatch
work, not arbitrary issue comments. Never register a webhook, send external
updates or expand a caller's permissions without the operator's authorization.

# Instabox Task Runner

Run coding tasks in isolated Instabox VMs or in guarded worktrees on one trusted
local host. Send a task from another device and continue in the same working
branch and Codex conversation.

```text
Phone / T3 Code ← private HTTPS → Go controller ← SSH → Instabox task VM
                          ↑                              T3 + Codex
                       your bot
```

Use any issue tracker or a plain prompt. A task ID is just your chosen identifier,
such as `fix-login` or `GH-42`. No issue-tracker account, API key or integration is
required. PR creation and issue updates are handled by your agent, not this CLI.

**Experimental.** Automated controller, credential-store and build checks pass on
macOS and Linux. The complete phone-to-bot-to-PR workflow, real host recovery and
long-term token refresh still need live verification. v0.1.0 is an unpublished draft.

## Install on your bot's host

Requires Go 1.26.10+ and an installed, authenticated [Instabox CLI](https://instabox.sh).
The controller and skill need no Python runtime.

```sh
git clone https://github.com/kevinsslin/instabox-ticket-runner.git
cd instabox-ticket-runner
go build -o bin/ib-ticket .
export PATH="$PWD/bin:$PATH"
ib-ticket --help
```

## Local Windows host preview

The local host backend replaces each task VM with a real Git worktree on the
controller machine. T3 remains the remote interface and starts Codex with the
host's existing login. Copy `config.local.example.yaml` outside the checkout,
replace its paths, and keep the same private config for every command.

```text
Mac, phone or bot → private T3 URL → Windows controller → local worktree
                                                   └── T3 → Codex App Server
```

```powershell
ib-ticket create --task fix-login
ib-ticket prepare fix-login

# Keep this foreground command under a trusted Windows service manager.
ib-ticket serve-local fix-login

# Run from another terminal or bot process.
ib-ticket run fix-login --model gpt-6.1-sol --prompt-file C:\private\task.txt
ib-ticket status fix-login
```

The underlying Codex conversation appears in Codex App when T3 and the app use
the same Codex home. Codex permits one writer at a time. To take over locally,
stop `serve-local`, then run `ib-ticket handoff fix-login --to app`. To return
control, release the conversation in Codex App, start `serve-local`, then run
`ib-ticket handoff fix-login --to t3`. The controller refuses T3 dispatch while
the recorded owner is the app. App-created turns are not imported into T3's own
message database, so inspect both histories after a handoff.

Local mode is currently one serialized Codex profile and one T3 listener per
task, run either in the foreground or as the Windows scheduled service below. It does not copy credentials,
checkpoint a VM, or require Instabox. The controller fetches the configured base
from `origin` and creates a new task branch beneath `local_worktree_root`.

On Windows, install the prepared task as a restartable per-user scheduled service
and expose only that listener to the private tailnet:

```powershell
pwsh -File deploy/install-local-task.ps1 `
  -BinaryPath C:\private\ib-ticket.exe `
  -ConfigPath C:\private\config.yaml `
  -StatePath C:\private\state `
  -TaskId fix-login `
  -LocalPort 3784 `
  -TailscaleHttpsPort 10000
```

The service runs after that user signs in, restarts after failure, and preserves
controller state, worktrees, and T3 history when removed. Use
`deploy/uninstall-local-task.ps1` to remove the scheduled task without deleting
task data. It intentionally preserves the Tailscale listener
because other local services may share that node configuration. Pairing
credentials must still be written with `pair --output-file` and opened privately.
The scheduler launches `ib-ticket.exe` directly and does not persist T3 startup
output because that output contains a bootstrap pairing credential.
Configure `local_t3_bin` with T3's real versioned `t3.exe` for the scheduled
service; the interactive `t3.cmd` launcher can detach its child without a console.

For a new trusted Windows host, an agent with PowerShell access can perform the
complete idempotent installation from this checkout. The bootstrap detects the
installed Codex, T3, Git, Go and Tailscale executables, builds the controller,
creates private config only when absent, prepares the worktree and installs the
scheduled service. It never performs Codex login or browser pairing.

```powershell
pwsh -File deploy/bootstrap-local-windows.ps1 `
  -LocalRepoRoot C:\src\your-repository `
  -TaskId remote-dev `
  -LocalPort 3784 `
  -TailscaleHttpsPort 10000 `
  -SkillRoot "$HOME\.grok\skills"
```

Existing config, controller state and task worktrees are preserved. A conflicting
Tailscale HTTPS port fails without replacing its current listener. Run the script
as the same signed-in Windows user that owns the Codex login. See
[Windows local bot self-install](docs/operations.md#windows-local-bot-self-install)
for agent instructions, verification and handoff rules. An existing installed
binary is reused; pass `-Rebuild` only during an intentional controller upgrade
after stopping its scheduled task.

`-SkillRoot` is optional. Point it at the current agent's documented skill
directory to create a Windows junction without copying or replacing an existing
skill. Grok commonly uses `$HOME\.grok\skills`; Codex commonly uses
`$HOME\.codex\skills`.

## Configure and log in

```sh
mkdir -p "$HOME/.config/instabox-ticket-runner"
chmod 700 "$HOME/.config/instabox-ticket-runner"
export IB_TICKET_CONFIG="$HOME/.config/instabox-ticket-runner/config.yaml"
test -e "$IB_TICKET_CONFIG" || cp config.example.yaml "$IB_TICKET_CONFIG"
chmod 600 "$IB_TICKET_CONFIG"
```

Edit the YAML with your profile, Instabox workspace and repository. The example
uses encrypted storage for a headless host; its key stays outside the checkout.
Keep this configuration in your bot's service environment for every command.

```sh
ib-ticket init-key
ib-ticket bootstrap
instabox ssh VM_ID -- 'codex login --device-auth'
ib-ticket checkpoint AUTH-YOUR_PROFILE
```

Bootstrap returns the login VM ID and auth task ID. Complete the official login
in your browser, then checkpoint that exact task. `init-key` creates a new private
key without printing it and refuses to replace one. Existing installations should
reuse their provisioned key. [Operations](docs/operations.md) covers desktop
stores, private repositories, configuration and recovery.

## Run a task with T3 Code

Start `ib-ticket supervise` as a persistent host service using the commands below
before dispatch. Write the request and acceptance criteria into a private text
file on the host.

```sh
ib-ticket create --task fix-login
ib-ticket prepare fix-login
ib-ticket run fix-login --model gpt-6.1-sol --prompt-file /private/task.txt
ib-ticket status fix-login

# With the host supervisor installed, checkpoint runs automatically.
# Inspect one recorded turn (when no supervisor daemon is running):
# Start it with run ... --supervision external, then use:
# ib-ticket watch fix-login
```

With `backend: t3`, `run` returns the original T3 thread/run IDs and a task URL
when `t3_public_url` is configured. The first run needs an explicit Codex model;
revisions reuse it. The VM runs pinned T3 0.1.6 and the controller remains Go.
Open the private T3 UI to read the conversation, inspect tools/diffs, answer
approvals, and send follow-up prompts to that same thread. No UI fork is bundled.

Start the gateway after creating the task:

```sh
ib-ticket ui fix-login --password-file "$HOME/.config/instabox-ticket-runner/dashboard-password"
# Inspect existing Serve mappings first; choose an unused HTTPS port.
tailscale serve --https=8443 --bg http://127.0.0.1:8788
```

Use the password setup below and set `t3_public_url` to the actual private HTTPS
origin before dispatch. Open `/_ib/connect` on that origin once to pair this
browser, then use the task link. Pairing happens privately in the browser; the
bot never needs to send a token in chat. T3 sessions and gateway authentication
are separate. After automatic pause, reload the task page and press **Resume in
T3 Code**. This restores login and the same conversation. Revisions through the
bot use another `run` with the same task ID.

For a persistent gateway, install [the task service](docs/operations.md#t3-code).
Each task gateway needs its own origin/listener/config; the supplied service is
one task slot. Do not point a saved task URL at a different task VM.

For the original backend, set `backend: native` in private YAML. It uses native
Instabox agent conversations and the optional read-only dashboard below. Native
Claude remains available; this T3 adapter currently supports Codex only. Existing
native tasks keep their backend and cannot be converted in place.

Start `ib-ticket supervise` as a persistent host service before dispatching.
It records and watches exact turn/job IDs, then checkpoints automatically after
completion. `run` requires a running supervisor by default and returns immediately; it does
not launch a hidden daemon.
For a foreground alternative, start `run ... --supervision external` followed by
`watch TASK_ID`. Phone disconnection is harmless
when the host service stays running. One Codex profile executes serially.

On Linux with systemd user services:

```sh
mkdir -p "$HOME/.local/bin" "$HOME/.config/systemd/user"
install -m 755 bin/ib-ticket "$HOME/.local/bin/ib-ticket"
cp deploy/ib-ticket-supervise.service "$HOME/.config/systemd/user/"
systemctl --user daemon-reload
systemctl --user enable --now ib-ticket-supervise
systemctl --user status ib-ticket-supervise
```

The template expects the private config at the quickstart path above. Use your
host's existing service manager on other systems. Check service startup before
sending tasks. To survive user logout, the host must support persistent user
services (systemd linger, configured by its administrator).

For native tasks, completion uses an `agent events -f` follower per turn. Startup and
stream closure trigger small status reads, with a five-minute safety reconciliation
for stalled connections. Verified completion triggers live safety checks,
observation capture and credential storage. There is no fixed stop-time guarantee
during provider/storage failures. New VMs use a 5-minute idle backup on paid plans; free
plans require `auto_suspend: 30m`. The existing 60-minute maximum runtime remains.
Idle/max-runtime pause does not save credentials or release the profile lease.
The controller scans local SQLite for new host-managed turns every 5 seconds;
it does not repeatedly download remote events while a follower is connected.
Disconnected followers use bounded backoff and jitter. The host supervisor fails
closed and marks attention when completion or storage is uncertain. Never automatically retry a lost agent start.

## Optional native task dashboard

The built-in Go dashboard shows tasks, VM/agent state, native messages and tool
results, command output/exit codes, tracked diffs and a recorded PR link. It is
read only. There is no frontend build or separate database service.

Generate a separate dashboard password on the bot host, outside the checkout:

```sh
if ! test -e "$HOME/.config/instabox-ticket-runner/dashboard-password"; then
  (umask 077; set -C; openssl rand -hex 24 > "$HOME/.config/instabox-ticket-runner/dashboard-password")
fi
ib-ticket serve --password-file "$HOME/.config/instabox-ticket-runner/dashboard-password"
```

Open `http://127.0.0.1:8787` on the host, or put this loopback listener behind your
HTTPS reverse proxy for phone access. Sign in as `operator` with that password.
Keep the password in a password manager; never include it in a task URL or chat.
Give the bot your dashboard's HTTPS base URL and have it return
`https://YOUR_HOST/#TASK_ID` (URL-encode the task ID). No model or Instabox tokens
are sent to the browser. See [dashboard operations](docs/operations.md#dashboard).

```sh
ib-ticket link-pr fix-login --url https://github.com/YOUR_ORG/YOUR_REPO/pull/123
```

The bot records a PR only after verifying it exists and belongs to the task.
The dashboard checks the repository in the URL, but does not verify PR contents.
Polling does not resume paused VMs. Viewing an active VM can count as activity;
close the page when finished. Large events/diffs are explicitly omitted or
truncated. Logs and diffs are private work, visible to anyone with dashboard access.

After checkpoint, the latest bounded events and diff remain available as a
private saved observation. The page labels it as historical, not live.

For a persistent Linux dashboard and private phone access, see
[host services and Tailscale](docs/operations.md#host-services-and-tailscale).

## T3 lifecycle limits

T3's browser streams its own conversation. The Go supervisor's T3 lifecycle
follower receives Linux filesystem notifications for T3's durable SQLite events;
it does not use `t3 --wait` polling or claim an upstream server subscription.
A five-minute safety read covers missed notifications. Before checkpoint, a
Linux systemd/cgroup-v2 guest guard freezes input and providers, verifies all
runs, queued/pending work and finalization, then stops the idle process group.
The normal encrypted-save/readback, auth removal, VM pause and lease rules still
apply. Unknown schema, background jobs or unavailable credentials preserve work.

This adapter is pinned to T3 0.1.6 internals. Upgrade and test the guard together
with T3; do not independently update a task server. The UI is the original T3
web app behind a private gateway. Native iOS/Android app pairing through this
Basic-auth gateway and the complete phone-to-Grok-to-PR flow remain unverified.
See [T3 operations](docs/operations.md#t3-code) for setup, recovery and constraints.

## Optional Linear agent

Run `ib-ticket linear --settings /PRIVATE/linear.yaml` on your controller host.
Use [linear.example.yaml](linear.example.yaml) with your own workspace, agent,
team/user allowlists and secret-file paths. Each installation uses the fixed
repository/profile in your normal private config. No company settings are bundled.

Delegate an issue or mention the installed agent in Linear. Its native Agent
Session receives an acknowledgement and monitor link. Replies in that agent
conversation become durable follow-up prompts for the same issue VM/branch and
conversation. They wait for earlier work. The worker returns final agent text,
checkpoints, pauses and reports the result. Verified PR metadata is linked too.
It does not execute every ordinary issue comment or automatically publish PRs.

Setup requires a registered Linear OAuth app installed as an agent, a provisioned
agent access token and a public HTTPS webhook proxy. OAuth installation/refresh
is managed outside this binary; this is not a one-click marketplace installation.
See [Linear operations](docs/operations.md#linear-agent). The dashboard and CLI
continue to work without Linear.

## Give it to your bot

For a bot that should operate this Windows computer directly without Instabox,
give it the repository URL plus this instruction:

> Install this repository on my trusted Windows Codex host. Read README.md,
> skills/instabox-ticket/SKILL.md and the Windows local bot self-install section
> in docs/operations.md. Do not install or create an Instabox VM. Preserve all
> existing config, state, worktrees, services and Tailscale listeners. Run
> deploy/bootstrap-local-windows.ps1 with the repository path and task ID I give
> you. Do not perform login, expose Funnel, print pairing credentials or send a
> task twice after an uncertain response. Verify the scheduled task, loopback T3
> endpoint, private Tailscale URL, controller status and one harmless Codex turn.
> Return the task ID, worktree, owner, conversation ID and private URL.

This requires the bot to have a command-execution tool on the Windows host. The
T3 browser URL is the human conversation surface, not a general remote shell.
An agent running elsewhere needs an existing trusted command bridge, such as its
host tool, restricted SSH or an allowlisted MCP wrapper around this CLI.

For an Instabox-backed host, use the original instruction below.

> Install this repository on your own persistent host. Read README.md and
> skills/instabox-ticket/SKILL.md, then follow the bot self-install procedure in
> docs/operations.md. Preserve existing config, keys and state. Use private YAML
> and encrypted-file storage. Guide me through official login without requesting
> tokens in chat. Use backend: t3 for new Codex tasks. Run the supervisor and
> task-specific T3 UI gateway as persistent services; use
> Tailscale Serve for private phone access when available. Verify a small task,
> automatic checkpoint and same-task revision while my laptop is offline, then
> return the monitor URL and evidence. Record verified PR links with link-pr;
> publish only within the task's scope. Preserve existing native tasks and leases.
> Do not convert them in place. Report missing capabilities or repository access.

Link [the skill directory](skills/instabox-ticket) into your agent's supported
skill discovery path after building the binary. For Grok installations using
`~/.grok/skills/`, link it there without replacing an existing installation.
The skill also works with other agents that can execute host commands.

[Operations](docs/operations.md) · [Security](SECURITY.md) ·
[CI](https://github.com/kevinsslin/instabox-ticket-runner/actions/workflows/test.yml)

MIT licensed. Community project, unaffiliated with Instabox or model providers.

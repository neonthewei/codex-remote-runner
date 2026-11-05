# Security

This tool trusts the controller host, its service account, Instabox and the coding
agent inside each task VM. A compromised host or malicious task code can access
credentials available to it. The CLI is for one trusted controller host, not a
multi-tenant service. Its lock is not distributed and profile names are not authentication.

The remote Windows mode also trusts the controller computer, the Windows account,
Tailscale and the pinned SSH host key. Its dedicated Ed25519 key is restricted by
`authorized_keys` to the same reviewed binary's worker entrypoint. The controller
sends one size-limited JSON request over standard input and cannot request a shell,
PTY or forwarding with that key. The worker accepts only named Git/T3 operations
and validates task paths against an operator-owned policy file. This bounds the
transport entrypoint; it does not sandbox Codex. A dispatched coding turn runs as
the Windows user and can exercise that account's filesystem and process authority.
Task ports must remain inside the operator-owned range. Managed stop requests are
written only beneath the validated task state directory and are consumed by the
worker process that owns that task's Windows Job Object; the controller cannot
submit an arbitrary PID to terminate. Port allocation is serialized by the
controller state lock, so one worker policy must not be shared by independent
controller state databases with overlapping ranges.
Compromise of the controller permits arbitrary task prompts, while compromise of
Windows can forge worker responses. Keep the policy and controller state private,
verify the host key out of band, restrict SSH to the tailnet and never use the T3
browser as a command channel. The supplied installer disables SSH password
authentication host-wide; review existing SSH users before applying it.

Codex profiles execute serially. Checkpoint reads back saved credentials before
removing guest auth and releasing a lease. Uncertain activity or failed storage
retains the lease and work. Never clear locks to bypass a failed handoff.

The encrypted-file backend uses AES-256-GCM with random nonces and authenticated
namespace/profile/version. It publishes immutable files with sync and private
permissions. The master key is a separate private file. Protect it independently
of bundle backups; encryption does not protect a compromised service account.
There is no automatic key rotation, backend migration or plaintext fallback.
See [operations](docs/operations.md#credential-storage) for provisioning and recovery.

Token bytes and encryption keys must stay out of YAML, prompts, argv, Git and logs.
Claude subscription credentials stay in each task VM. Limit GitHub Secret access,
disable personal Vault injection and use clean snapshots. Deleting guest auth does
not securely erase disk history, snapshots or copies created by task code.
Task prompts and agent events may contain private work; review before sharing.

The optional dashboard uses HTTP Basic authentication with a separate private
password file and binds only to loopback. Remote access requires an HTTPS proxy.
Every route is authenticated; responses are no-store and use a restrictive CSP.
The browser receives task metadata, native events and diffs, never credential-store
bundles. Agent output or repository changes can nevertheless contain secrets;
there is no guaranteed secret redaction. Treat dashboard access as access to all
private work on this controller, and never share the password publicly.
The server has a read-only SQLite connection and no browser mutation endpoints.
It checks VM ownership before collecting guest data and never resumes a VM.
Hostile task code can falsify command output or diffs; inspect PR evidence yourself.

The optional T3 gateway is an interactive surface. It binds to loopback behind
private HTTPS and requires the separate gateway password plus a T3 browser
session. Its fixed task/VM mapping is not browser-controlled. Only explicit,
same-origin, CSRF-protected actions resume a VM or mint a short-lived pairing;
background reconnects never resume it. Pairing credentials are delivered only to
the authenticated browser and consumed by T3. Pairing uses SSH so credential
output does not enter Instabox exec history. Do not share task origins between
unrelated task VMs. The original T3 web app controls its own content/security
policy. Native mobile apps through this gateway have not been verified.

T3 runs in a dedicated Linux systemd cgroup. The pinned guard reads T3's private
SQLite state without modifying it, freezes admissions and provider processes,
checks queued/pending/finalization work and unknown processes, then kills the
idle group before checkpointing credentials. It depends on T3 0.1.6's schema
and crash recovery; upgrade both together. Unsupported state/processes preserve
credentials and the lease. T3's service console is suppressed because its normal
startup prints pairing credentials. Its durable logs/transcripts and all task
disks remain private sensitive data, separate from encrypted login bundles.

The optional Linear bridge accepts only signed, fresh Agent Session callbacks
for configured workspace/app/team/human IDs. It persists delivery IDs and dispatch
intent to avoid automatic replay after uncertain operations. Its OAuth token and
signing secret stay in private host files. The bridge can execute code with the
configured profile/repository authority; allowlist membership is an authorization
boundary, and issue text cannot choose credentials or another repository. Native
agent output is sent back only to the configured Linear session. Untrusted issue
content can still prompt-inject the coding agent. The native CLI runs with VM
permissions, so scope repository tokens and review changes before merging.
Observation snapshots and Linear inbox/replies contain plaintext private work in
the protected state directory. They are separate from encrypted login bundles.

Report vulnerabilities privately to the repository owner, using GitHub private
vulnerability reporting when available. Do not post credentials or private state
in public issues. No guaranteed response time is offered.

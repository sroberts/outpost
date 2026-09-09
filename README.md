# Outpost

A small Go daemon that runs a declared set of local services as long-lived
processes, supervises them, and gives you a CLI to manage them.

The manifest is the source of truth: you declare services as TOML files, and
`outpost` reconciles what is running to match. Process supervision is Outpost's
own code — `launchd` is used only to keep the Outpost daemon itself alive.

Design rationale, and the parts not built yet, live in [`spec.md`](spec.md).
See [Deviations from the spec](#deviations-from-the-spec) for where this code
and that document disagree.

## Status

Built and tested: manifest loading and validation, process supervision with
restart policies and backoff, SQLite state, the CLI, the daemon and its control
socket, launchd registration, and backup/restore.

Not built: container services (`runtime = "container"` is rejected at apply
time), Hermes `SKILL.md` generation, and the TUI.

**Tailscale integration is written but unverified.** No node has ever joined a
real tailnet — it needs credentials and a device slot. It is off by default; see
[Tailscale](#tailscale).

## Building

Go 1.26 or newer. No cgo — the SQLite driver is pure Go, so the result is a
static binary.

```sh
go build -o outpost ./cmd/outpost
go test ./...
```

The binary is ~44 MB, most of which is the `tailscale.com` dependency.

## Quick start

```sh
mkdir -p ~/.outpost/services
cat > ~/.outpost/services/plumbline.toml <<'EOF'
binary  = "/opt/outpost/bin/plumbline"
args    = ["serve", "--port", "8081"]
port    = 8081
storage = "~/.outpost/data/plumbline"
tailscale_name = "plumbline"
EOF

outpost apply     # validate the manifests
outpost serve     # run the daemon (foreground)
```

Then, from another shell:

```sh
outpost list
outpost status plumbline
outpost logs plumbline -f
```

`outpost serve` runs in the foreground and stops every service it started when
it exits, so nothing is left orphaned holding a port. To keep it running across
logins and reboots, see [Running under launchd](#running-under-launchd).

## The service contract

Outpost does not care what a service does internally. It requires that the
service:

- listens on one TCP port, given as `port`
- serves a healthcheck at `healthcheck` (default `/up`) returning 2xx when healthy
- takes configuration from environment variables or CLI arguments
- writes persistent state under one directory, handed to it as `OUTPOST_STORAGE_DIR`
- exits non-zero on unrecoverable failure

Outpost injects the manifest's `env` plus `OUTPOST_STORAGE_DIR` into the
process environment. `OUTPOST_STORAGE_DIR` is appended last, so a manifest
cannot accidentally override it.

## Manifest schema

One TOML file per service in `~/.outpost/services/`. The filename minus its
extension is the service name unless `name` overrides it.

```toml
# ~/.outpost/services/plumbline.toml
name           = "plumbline"        # optional; defaults to the filename
runtime        = "process"          # "process" | "container"

# runtime = "process"
binary         = "/opt/outpost/bin/plumbline"
args           = ["serve", "--port", "8081"]
env            = { LOG_LEVEL = "info" }

# runtime = "container" — not implemented yet, rejected at apply time
# image          = "koreader/kosync:latest"
# container_args = []

port           = 8081                          # required
storage        = "~/.outpost/data/plumbline"   # required, created 0700 if absent
tailscale_name = "plumbline"                   # required, unique on the tailnet
funnel         = false                         # rejected if true
healthcheck    = "/up"
mcp            = true                          # informational only
hermes_skill   = true                          # informational until §11 is built
restart_policy = "on-failure"                  # "on-failure" | "always" | "never"
backoff_seconds     = 5
start_grace_seconds = 30
```

| Field | Default | Notes |
|---|---|---|
| `name` | filename | Must be unique |
| `runtime` | `process` | `container` is rejected at apply time |
| `port` | — | Required; must be unique across `process` services |
| `storage` | — | Required; `~` is expanded, created `0700` if missing |
| `tailscale_name` | — | Required; must be unique |
| `healthcheck` | `/up` | Must start with `/` |
| `restart_policy` | `on-failure` | See below |
| `backoff_seconds` | `5` | Doubles per consecutive restart, capped at 300s |
| `start_grace_seconds` | `30` | Healthcheck failures do not count during this window |
| `funnel` | `false` | `true` is rejected — deferred, not designed out |

`outpost apply` validates the whole set and fails loudly rather than
half-applying. It checks for duplicate names, ports and Tailscale names,
missing required fields, and runtime/field mismatches.

### Restart policies

| Policy | Behaviour |
|---|---|
| `on-failure` | Restart on a non-zero exit or a failed healthcheck |
| `always` | Also restart after a clean (zero) exit |
| `never` | Never restart automatically |

Backoff starts at `backoff_seconds`, doubles each consecutive restart, and caps
at 300s. It resets once a service has run healthily for 60 seconds, so a service
that crashes once an hour does not creep toward the cap.

A service the supervisor gives up on — `restart_policy = "never"`, or a health
failure under that policy — is marked **halted** and shows as such in
`outpost status`. Only an explicit `outpost start` or `outpost restart` resumes
it; a routine status update will not clear the halt.

### Startup grace

A service is not penalised for failing its healthcheck during the first
`start_grace_seconds` after it starts, and reports status `starting`. The window
ends at the first successful healthcheck, or when it expires. This exists so
that anything slow to bind its port — a database, a JVM service, anything
running migrations on boot — is not put into a permanent restart loop.

A crash during the grace window is still a crash, and is restarted per policy.
The grace covers slow healthchecks, not a process that exits.

## CLI

```
outpost serve [--tailnet]        run the daemon (reconcile loop + control socket)
outpost apply                    validate manifests and reconcile
outpost list                     table of services and their status
outpost status <service>         health, restart count and recent events
outpost logs <service> [-f]      show or follow a service's logs
outpost start <service>          start a stopped service
outpost stop <service>           stop a running service
outpost restart <service>        restart a service
outpost backup [service]         archive a service's storage (all if omitted)
outpost backup --list <service>  list existing backups
outpost restore <service> [ts]   restore from a backup (newest if ts omitted)
outpost install                  write the launchd agent
```

Global flags:

| Flag | Default | Notes |
|---|---|---|
| `--home <dir>` | `$OUTPOST_HOME`, else `~/.outpost` | Outpost root |
| `-h`, `--help` | | Show usage |

`serve` also takes `--interval` (default 3s), `--tailnet`, and
`--tailnet-verbose`. Flags may appear on either side of a service name:
`outpost status --events 20 web` and `outpost status web --events 20` both work.

Every command except `logs` talks to the running daemon over the control
socket, so the CLI and the daemon share one code path. `logs` reads the log file
directly — it is a local file, and streaming a follow over the socket would need
a framed protocol for no practical gain.

Commands that need the daemon fail with a clear error when none is running.
`apply` still validates manifests without one, since that is a pure function of
the files on disk.

## Layout

Everything lives under `~/.outpost` (or `$OUTPOST_HOME`, or `--home`):

```
~/.outpost/
  services/<name>.toml     service manifests — the source of truth
  data/<name>/             default location for service storage
  logs/<name>.log          combined stdout/stderr, rotated at 10 MiB, 3 kept
  backups/<name>/<ts>.tar.gz
  tsnet/<name>/            per-service Tailscale node identity
  state.db                 SQLite observed state and event log
  outpost.sock             control socket, mode 0600
  outpost.pid              running daemon's PID
```

`state.db` is a cache of observed reality, not the source of truth. Deleting it
and re-running `outpost apply` converges to the same running state; there is a
test for exactly that. The one thing it holds that is *not* a cache is your
intent: `outpost stop` records that a service should stay down.

> **Note on socket paths.** Unix domain sockets have a hard length limit in the
> kernel (104 bytes on macOS). `~/.outpost/outpost.sock` is well under it, but a
> deeply nested `--home` can exceed it; Outpost reports this explicitly rather
> than failing with a bare `bind: invalid argument`.

## Running under launchd

```sh
outpost install          # writes ~/Library/LaunchAgents/com.sroberts.outpost.plist
outpost install --print  # print the plist without writing it
outpost install --load   # write it and load it immediately
```

Without `--load`, `install` prints the `launchctl` commands rather than running
them — registering a background service on your machine should be your call.

```sh
launchctl load -w ~/Library/LaunchAgents/com.sroberts.outpost.plist
launchctl unload -w ~/Library/LaunchAgents/com.sroberts.outpost.plist
```

The agent runs with `KeepAlive`, so launchd restarts the Outpost daemon if it
dies; Outpost restarts the services. Its own stdout and stderr go to
`~/.outpost/logs/outpost.{out,err}.log`.

`install` is macOS-only. On Linux, run `outpost serve` under a systemd unit.

## Backups

```sh
outpost backup plumbline          # one service
outpost backup                    # every service, in turn
outpost backup --list plumbline
outpost restore plumbline
outpost restore plumbline 2026-09-08T23-04-11
```

A backup stops the service, tars its storage directory to
`~/.outpost/backups/<service>/<timestamp>.tar.gz`, and restarts it if it was
running. A service you had already stopped stays stopped.

Restore stops the service, replaces its storage from the archive, and restarts
it. **Restore discards anything written since the backup**, including files the
archive does not contain. The previous directory is kept until the new one is
in place, so a failed restore does not destroy the data it was replacing.

Archives are plain `tar.gz` and can be unpacked with standard tools. Extraction
refuses entries that escape the target directory, including absolute paths,
`../` traversal, and writes through a symlink the archive itself created.

There is no scheduler yet — use cron, or `launchd`. Whether Outpost grows its
own or delegates to Hermes's is open question 2 in the spec.

## Tailscale

**Unverified.** The code is written and unit-tested against local backends, but
no node has joined a real tailnet.

```sh
TS_AUTHKEY=tskey-... outpost serve --tailnet
```

Each service gets its own `tsnet.Server`, named by `tailscale_name`, with node
state under `~/.outpost/tsnet/<service>/` so identities persist across restarts.
The node listens on the tailnet and reverse-proxies to `127.0.0.1:<port>`, so
the supervised process never binds anything but loopback.

Without `TS_AUTHKEY`, tsnet logs a one-time login URL per service on first run.
Each service costs one device on your tailnet — check your plan's limit before
this scales past a handful. `funnel = true` is rejected.

Joining the tailnet happens in the background: `Attach` records intent and
returns, so a node that is slow to register — or waiting on a first-run login
URL — never stalls supervision or the control socket. A failed attach is retried
with backoff (2s, doubling to 60s) until it succeeds or the service is detached,
and the reconcile loop re-asserts membership for every running service, so a
service cannot end up silently off the tailnet.

## Development

```sh
go test ./...           # 160 tests
go test ./... -race
go vet ./...
```

Layout follows `spec.md` §4. `internal/manifest` parses and validates;
`internal/runtime` starts and stops things; `internal/supervisor` decides what
*should* run and owns the reconcile loop; `internal/state` records what was
observed; `internal/ipc` carries CLI requests to the daemon; `internal/backup`
and `internal/tsnet` are self-contained.

The supervisor's state is deliberately single-threaded. Its exported API hands
work to the reconcile loop through a command queue rather than locking, and runs
inline when no loop is running, so every method is safe to call from any
goroutine. `internal/runtime/testdata/fakeservice` is a real HTTP service that
can be told to crash or go unhealthy on demand, used by the runtime tests.

## Deviations from the spec

`spec.md` is the design document and has not been updated as the code was
written. Where the two disagree, this section is current.

**`start_grace_seconds` is new.** Not in spec §5. Added because the reconcile
loop began healthchecking immediately after start, which put any slow-starting
service into a permanent restart loop.

**The state store has three extra columns.** Spec §8 lists `services(name,
runtime, status, pid_or_container_id, last_started_at, restart_count,
updated_at)`. This adds `desired` (your start/stop intent), `restart_epoch` (how
`outpost restart` reaches the daemon) and `halted` (whether the supervisor has
given up). They are load-bearing, not incidental.

**The CLI differs from spec §12.** Added: `serve`, `install`, `--home` /
`$OUTPOST_HOME`, `backup --list`, `--tailnet`. Not built: `outpost tui` and
`outpost hermes publish`, which spec §12 lists.

**Manifest reloading polls instead of using fsnotify.** Spec §6 calls for
fsnotify. The daemon hashes the manifest directory each tick and re-applies on
change, which avoids a dependency; fsnotify would only reduce reload latency
below one tick.

**`funnel` and containers are rejected, not ignored.** Both are accepted by the
schema and refused at apply time with an explicit error, per spec §7 and §9.

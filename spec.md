# Outpost — Spec

Status: draft, ready to build against
Author: Scott Roberts (drafted with Claude)
Date: 2026-09-08

## BLUF

Outpost is a small Go daemon that runs a declared set of local services (static Go binaries and, where needed, OCI containers) as long-lived processes, gives each one a stable Tailscale identity via `tsnet`, and gives you a CLI/TUI to manage them. It replaces Basecamp's ONCE for your own stack: same idea (declare an app, get it running, reachable, backed up, restartable) without the Docker Desktop dependency, the 37signals app catalog, or a NixOS-specific supervision layer that won't survive a platform change.

Target: your Mac Mini now, a Linux box later, no rewrite between them. Process supervision is Outpost's own Go code, not a wrapper around systemd or launchd — those are used only to keep the Outpost daemon itself alive across a reboot.

Non-goals: Outpost is not a DNS/TLS manager (Tailscale covers that), not a general PaaS, not an agent framework. It manages processes and containers; Hermes (already running on the same box) handles agent behavior, and the two integrate through generated `SKILL.md` files, not a private API.

## 1. Scope

### In scope, v1

- Run and supervise declared services, each either a native process or an OCI container
- Put each service on the tailnet with its own MagicDNS name via `tsnet`
- Healthcheck-based status (`GET /up` or equivalent)
- Structured logs per service, retained locally
- CLI to add/remove/start/stop/restart/list/inspect services
- Manifest-driven, declarative: the manifest is the source of truth, `outpost apply` reconciles running state to it
- Backups of each service's declared storage path
- Generate a Hermes-compatible `SKILL.md` per service on request

### Out of scope, v1 (explicitly deferred, not designed out)

- Funnel (public internet exposure) and the OAuth work that comes with it
- Multi-host / clustering — one Outpost instance manages one machine
- A web UI — TUI and CLI only
- Auto-update of managed binaries/images (pull latest manually or via your own tooling for now)

## 2. Service contract

Outpost doesn't care what a service does internally. It requires:

| Requirement | Detail |
|---|---|
| Listens on one TCP port | HTTP, provided via manifest `port` (process) or container `EXPOSE`/port-map |
| Healthcheck endpoint | Path configurable per service (default `/up`), must return 2xx when healthy |
| Reads config from env or flags | Outpost injects env vars and/or CLI args from the manifest; no shared config file format required |
| Writes persistent state under one directory | The manifest's `storage` path, bind-mounted for containers, passed as an env var (`OUTPOST_STORAGE_DIR`) for processes |
| Exits non-zero on unrecoverable failure | Drives Outpost's restart-on-failure policy |

Any personal-utils binary built with a `serve` subcommand (REST at `/v1/*`, MCP at `/mcp`, healthcheck at `/up`) satisfies this automatically — that's a separate spec, but Outpost's manifest has an `mcp` flag specifically so it knows whether a service exposes an MCP endpoint worth advertising (e.g., in a generated Hermes skill or future MCP-aggregation work). A third-party container that only speaks its own REST API still works; set `mcp = false`.

## 3. Architecture

```
                     ┌─────────────────────────┐
                     │      outpost (daemon)     │
                     │                          │
  ~/.outpost/         │  ┌────────────┐  ┌──────┴───┐
   services/*.toml ───┼─▶│  Manifest  │  │  State   │
                     │  │  Loader    │  │  Store   │
                     │  └─────┬──────┘  │ (SQLite) │
                     │        │         └──────┬───┘
                     │        ▼                │
                     │  ┌────────────┐         │
                     │  │ Supervisor │◀────────┘
                     │  │  (reconcile│
                     │  │   loop)    │
                     │  └─────┬──────┘
                     │        │
                     │  ┌─────┴───────────────┐
                     │  ▼                     ▼
                     │ ProcessRuntime    ContainerRuntime
                     │ (os/exec)         (podman/colima)
                     │  │                     │
                     │  ▼                     ▼
                     │ ┌────────────────────────┐
                     │ │   tsnet router          │  one tsnet.Server per
                     │ │   (Tailscale identity)  │  service, or shared —
                     │ └────────────────────────┘  see §7
                     │        │
                     │        ▼
                     │  Hermes SKILL.md
                     │  generator (writes into
                     │  Hermes's skills dir)
                     └─────────────────────────┘
        ▲                                  ▲
        │                                  │
  outpost CLI                        outpost-tui (Bubble Tea)
  (talks to daemon over               (same, richer view;
   a local unix socket)                absorbs selfhost-tui scope)
```

Single binary, `outpost`, with subcommands for CLI use and a `serve`/default mode that runs the daemon. The TUI is a build target of the same module (`cmd/outpost-tui` or a `--tui` flag) so there is one Go module, one release artifact family, matching your existing personal-utils convention of one binary with multiple interfaces.

## 4. Repo layout

```
outpost/
  cmd/
    outpost/            # main CLI + daemon entrypoint
  internal/
    manifest/           # TOML parsing, validation, schema (§5)
    supervisor/          # reconcile loop, restart policy (§6)
    runtime/
      process.go         # ProcessRuntime
      container.go        # ContainerRuntime (podman/colima backend)
      runtime.go          # shared Runtime interface
    tsnet/               # tsnet.Server lifecycle per service (§7)
    state/               # local state store (SQLite) (§8)
    backup/              # tar-based backup/restore (§10)
    hermes/               # SKILL.md generation (§11)
    tui/                 # Bubble Tea dashboard
  docs/
    SPEC.md              # this file, kept in the repo and updated as design changes
  go.mod
```

## 5. Manifest schema

One TOML file per service under `~/.outpost/services/`. Filename (minus extension) is the service name unless overridden.

```toml
# ~/.outpost/services/plumbline.toml
name           = "plumbline"          # required, unique
runtime        = "process"            # required: "process" | "container"

# runtime = "process"
binary         = "/opt/outpost/bin/plumbline"
args           = ["serve", "--port", "8081"]
env            = { LOG_LEVEL = "info" }

# runtime = "container" (mutually exclusive with binary/args above)
# image        = "koreader/kosync:latest"
# container_args = []

port           = 8081                 # required: port the service listens on
storage        = "~/.outpost/data/plumbline"   # required, created if absent
tailscale_name = "plumbline"          # required, must be unique on the tailnet
funnel         = false                # default false; v1 does not implement true
healthcheck    = "/up"                # default "/up"
mcp            = true                 # informational: does this service expose /mcp
hermes_skill   = true                 # generate a SKILL.md for this service
restart_policy = "on-failure"         # "on-failure" | "always" | "never"
backoff_seconds = 5                    # initial restart backoff, doubles to a cap of 300s
```

Validation rules `internal/manifest` enforces at load time (fail `outpost apply` loudly, don't half-apply):

- `name` and `tailscale_name` unique across all loaded manifests
- `port` unique across all `runtime = "process"` services (containers get their own network namespace, so container ports don't need host-level uniqueness the same way — still warn on collision against the host if `funnel` or a manual bind is added later)
- exactly one of (`binary`+`args`) or `image` is set, matching `runtime`
- `storage` path must be writable; created with `0700` if missing
- `healthcheck` must start with `/`

## 6. Supervisor

Core loop, roughly:

```go
type Runtime interface {
    Start(ctx context.Context, svc *manifest.Service) (Handle, error)
    Stop(ctx context.Context, h Handle) error
    Healthy(ctx context.Context, h Handle) (bool, error)
    Logs(h Handle) (io.ReadCloser, error)
}

type Handle interface {
    PID() int          // or container ID, exposed uniformly
    Status() Status     // Starting | Running | Unhealthy | Stopped | Failed
}
```

`ProcessRuntime.Start` does `exec.CommandContext`, wires stdout/stderr to a rotating log file under `~/.outpost/logs/<service>.log`, and records the PID in the state store. `ContainerRuntime.Start` shells out to the chosen engine's CLI (`podman run -d --name outpost-<service> --restart=<policy> -v <storage>:/data -p 127.0.0.1:<port>:<container-port> <image> <container_args>`), since driving Podman's CLI directly is simpler and more debuggable than binding its Go client library for a v1.

Reconcile loop (`supervisor.Run`, ticks every few seconds and on manifest-change fsnotify events):

1. Diff loaded manifests against state store's last-known set → start new, stop removed, restart changed
2. For each running service, poll `healthcheck`; on N consecutive failures, restart per `restart_policy` with exponential backoff (`backoff_seconds`, doubling, capped at 300s)
3. On process exit, same restart logic, distinguishing clean stop (user-initiated, don't restart) from crash (restart per policy)
4. Write status transitions to the state store so `outpost status` and the TUI have something to read without touching the runtime directly

## 7. Tailscale integration

One `tsnet.Server` per service (`internal/tsnet`), started when the supervisor starts the service, using `tailscale_name` as the hostname and a state directory under `~/.outpost/tsnet/<service>/` so each service's node identity persists across Outpost restarts instead of re-registering every time. The `tsnet.Server` listens on the tailnet and reverse-proxies to the service's local port (`127.0.0.1:<port>`), so the underlying process/container never binds anything but loopback — nothing is reachable except through Tailscale, which is the whole point.

`funnel = true` is accepted by the schema but rejected at `outpost apply` time in v1 with a clear error pointing at this doc's open questions — build it deliberately later, once the MCP auth question (see the parent server-mode spec, Part 1 §4) is actually answered, not as a side effect of a manifest flag.

Per-service nodes cost one tailnet device each; check your plan's device limit before this scales past a handful of services. If that becomes a problem, the fallback is one shared `tsnet.Server` for Outpost itself with path-based routing (`outpost.<tailnet>.ts.net/plumbline/...`) — noted here as the fallback design, not built for v1.

## 8. State store

SQLite (`~/.outpost/state.db`, via `modernc.org/sqlite` for a pure-Go driver — no cgo, keeps the static-binary property). Tables, roughly:

- `services (name, runtime, status, pid_or_container_id, last_started_at, restart_count, updated_at)`
- `events (id, service, kind, message, at)` — a rolling log of start/stop/restart/healthcheck-fail events, bounded (prune past N days or M rows), what `outpost logs --events` and the TUI's activity feed read from

State store is a cache of observed reality, not the source of truth — the manifest is. A corrupted or deleted state DB should be recoverable by just re-running `outpost apply`, which is worth a test in the test suite (§13).

## 9. Container runtime

Pick one engine for v1 and treat the choice as a config value, not an abstraction to build now: `container_engine = "podman"` in `~/.outpost/config.toml` (global, not per-service), defaulting to whichever engine Hermes's own Docker execution backend already uses on the box (check this before writing code — see the parent spec's sequencing step 2). `internal/runtime/container.go` shells out to `<engine> run/stop/inspect/logs`, so swapping engines later is a matter of adjusting flag differences in one file, not a rewrite.

Do not add a Docker-Desktop-specific code path. If Docker Desktop's daemon happens to be present and the engine is set to `docker`, it'll work because both Podman and Colima expose Docker-CLI-compatible interfaces — but don't special-case it, and don't make it the default.

## 10. Backups

`outpost backup [service]`: for each service (or the one named), stop it, `tar czf` its `storage` directory to `~/.outpost/backups/<service>/<timestamp>.tar.gz`, restart it. `outpost restore <service> <timestamp>`: stop, wipe `storage`, untar, restart. No hook-script mechanism (ONCE's pre-backup/post-restore) in v1 — add per-service hooks only when a specific service needs consistent-snapshot semantics (a database that needs a checkpoint before a filesystem-level copy is safe, for instance).

Scheduling: cron (system cron, or a manifest-level `backup_schedule` cron expression that Outpost's own scheduler reads) vs. delegating to Hermes's built-in cron scheduler is an open question (§14) — don't build Outpost's own scheduler until that's decided, since duplicating it after picking Hermes's would be wasted work.

## 11. Hermes integration

`outpost hermes publish [service]` (also run automatically by `apply` for any service with `hermes_skill = true`): generates a `SKILL.md` describing the service's REST API (endpoint list, request/response shapes) at its Tailscale address, and writes it to Hermes's skills directory.

Two things to nail down before this is real, both one-time investigation against the actual Hermes install on the Mac Mini, not assumptions to code against:

1. **Where Hermes reads skills from.** Likely under `~/.hermes/`, not documented publicly. Inspect the running install.
2. **The exact SKILL.md shape Hermes expects**, beyond "it's the open SKILL.md format" — check an existing installed Hermes skill as a template rather than inventing the frontmatter/structure from scratch.

Source for the generated content: same annotated tool/endpoint descriptions the service's own OpenAPI doc and MCP tool definitions come from (per the parent spec, Part 1 §3–4) — Outpost's generator should consume that service's `openapi.yaml` (or an equivalent machine-readable source each `serve`-mode service is expected to expose, e.g. at `GET /v1/openapi.json`) rather than requiring per-service, hand-written Hermes-skill content in the Outpost manifest.

## 12. CLI reference

```
outpost apply                 # reconcile running state to manifests (start/stop/restart as needed)
outpost list                  # table: name, runtime, status, uptime, tailscale name
outpost status <service>      # detail view: health, restart count, recent events
outpost logs <service> [-f]   # tail or follow
outpost start <service>
outpost stop <service>
outpost restart <service>
outpost backup [service]
outpost restore <service> <timestamp>
outpost hermes publish [service]
outpost tui                   # launch the Bubble Tea dashboard
```

Every subcommand talks to the running `outpost` daemon over a local Unix socket (`~/.outpost/outpost.sock`) rather than touching the state store or manifests directly, so the CLI, the TUI, and any future integration all go through one code path (`internal/supervisor`'s public API) instead of duplicating logic — same "one core, multiple interfaces" principle the parent spec applies to individual utilities, applied here to Outpost itself.

## 13. Testing

- `internal/manifest`: table-driven validation tests (duplicate names, port collisions, missing required fields) — write these first, they're cheap and catch the most common real mistakes
- `internal/supervisor`: reconcile-loop tests against a fake `Runtime` (no real processes/containers), covering start/stop/restart/backoff transitions
- `internal/runtime/process.go`: integration tests against a tiny real test binary (a `testdata/fakeservice` that serves `/up` and can be told to crash on demand) — verifies restart policy and log capture against a real process
- `internal/runtime/container.go`: gated behind a build tag or `-tags integration`, since it needs the container engine actually installed; skip in CI if the engine isn't present, don't fake this layer
- `internal/tsnet`: exercise against Tailscale's test/ephemeral node support if available, otherwise keep this layer thin enough that manual verification is acceptable for v1
- State-store recovery: explicit test that deleting `state.db` and re-running `apply` converges to the same running state

Per your stated methodology: write these before the corresponding implementation, change them only with explicit sign-off once written.

## 14. Open questions

1. **Container engine**: Podman vs. Colima — decide by checking what Hermes's Docker execution backend uses first.
2. **Backup scheduling**: Outpost's own cron vs. delegating to Hermes's built-in scheduler.
3. **Per-service vs. shared tsnet identity**: per-service is the v1 design above; revisit if tailnet device limits bite.
4. **Where Outpost itself runs**: `launchd` agent that only runs at `apply` time (thin, mostly declarative) vs. a persistent daemon (required for the Unix-socket CLI model in §12 — leaning toward persistent daemon, confirm before building the CLI's transport layer).
5. **Hermes skills directory path and SKILL.md shape**: unknown until inspected on the actual box (§11).
6. **Naming**: Outpost is a placeholder in the Handler/Subrosa/APT-C-Monitor naming register.

## 15. Build order

1. `internal/manifest` + validation tests
2. `internal/runtime` (process only) + `internal/supervisor` reconcile loop, tested against the fake runtime and the real test binary
3. `internal/state` (SQLite store), wired into the supervisor
4. `cmd/outpost` CLI skeleton (`apply`, `list`, `status`, `logs`, `start`, `stop`, `restart`) talking to an in-process supervisor (no daemon/socket split yet) — get the core loop provably correct before adding IPC
5. Daemon + Unix-socket transport, splitting CLI from the running supervisor process; add `outpost.plist` for `launchd` self-registration
6. `internal/tsnet` — one service (Plumbline, once it has `serve` mode) reachable over Tailscale end to end
7. `internal/backup`
8. `internal/runtime/container.go`, proven against kosync, once the engine decision (open question 1) is made
9. `internal/hermes`, once the Hermes install has been inspected (open question 5)
10. TUI (`internal/tui`), absorbing [[selfhost-tui]]'s planned scope

Each step should be a mergeable PR under GitHub Flow, not one long-lived branch — the ordering above is also roughly the PR sequence.

## Sources

- [basecamp/once](https://github.com/basecamp/once)
- [tsnet package docs](https://pkg.go.dev/tailscale.com/tsnet)
- [tsnet · Tailscale Docs](https://tailscale.com/kb/1244/tsnet)
- [Hermes Agent](https://hermes-agent.org/)
- [What Is Hermes Agent? Self-Hosting & Task Automation in 2026](https://www.digitalocean.com/resources/articles/what-is-hermes-agent)
- Parent spec: `personal-utils-server-framework-spec.md` (Part 1 defines the `serve` mode / MCP contract Outpost-managed services are expected to satisfy)
# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Outpost is a Go daemon that supervises a declared set of local services and
optionally gives each one its own Tailscale identity. Services are declared as
TOML manifests in `~/.outpost/services/`; the daemon reconciles reality to match
them. `spec.md` is the design document and `outpost-service-spec.md` is the
contract a managed service has to satisfy.

## Commands

`make help` lists everything. `make check` is exactly what CI runs — use it
before opening a PR.

```sh
make build                                       # ./outpost
make test                                        # full suite (261 tests)
make test-race                                   # race detector; the suite is clean, keep it that way
make lint                                        # go vet + golangci-lint
make fmt-check                                   # gofmt, honouring the one exempt file
make check                                       # everything CI runs

go test ./internal/supervisor -run TestName -v   # one test
```

Cross-compiling must stay cgo-free (see below):

```sh
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o outpost ./cmd/outpost
```

The supervisor's test fixture is a real binary, not a mock —
`internal/runtime/testdata/fakeservice` serves `/up` and can be told to go
unhealthy, crash, or ignore SIGTERM. Reach for it when a change needs proving
against a real process.

## Architecture

**One core, multiple interfaces.** `internal/supervisor` holds all the logic.
The CLI, the TUI, and anything added later go through its public API over a Unix
socket — never by reading `state.db` or manifests directly. `cmd/outpost serve`
is the daemon; every other subcommand is a client.

**The manifest is the source of truth; `state.db` is a cache of observed
reality.** Deleting the state store and re-running `apply` must converge to the
same running set (`TestStateRecovery_ReapplyConvergesAfterStateLoss`). Don't
introduce logic that treats the database as authoritative.

**The reconcile loop is single-threaded, and that is load-bearing.** `Run` ticks
on a timer and drains a command channel; API calls don't touch supervisor state
directly, they hand a closure to `submit()` and wait for the loop to run it. Any
new API method must follow that pattern. When the loop isn't running, `submit`
executes inline under `loopMu` — `loopRunning` alone is not enough, because a
caller can read it as false in the instant before `Run` sets it. That was a real
data race, and there is a regression test for it.

**Package roles:** `manifest` (schema + validation) · `runtime` (process
execution, process groups, rotating logs) · `supervisor` (reconcile loop,
restart policy, backoff, orphan reclamation) · `state` (SQLite) · `ipc` (JSON
over a 0600 Unix socket) · `tsnet` (per-service Tailscale nodes) · `backup`
(tar.gz) · `autostart` (launchd/systemd units) · `config` · `tui`.

## Invariants that bite

**Never run `gofmt -w` on a directory.** `internal/manifest/manifest_test.go` is
deliberately left unformatted and `gofmt -l .` is expected to report it and
nothing else. A directory-wide sweep silently reformats it. Use `make fmt`,
which carries the exemption, or format the specific files you edited.

**The tree is `golangci-lint` clean, errcheck included.** A deliberately
discarded error is written `_ = f()` at the call site with a comment saying
why, not left bare. Inside `internal/supervisor`, activity-feed writes go
through `s.event(...)` rather than `s.store.AppendEvent(...)` — it logs a
failed write instead of dropping it.

**Tests are not yours to change.** Per the repository owner's standing
instruction, existing tests may only be modified with explicit authorization.
Adding new tests is fine; editing or deleting existing ones needs a yes first.
If a change makes an existing test fail, that is a signal to reconsider the
change, not the test.

**Stay pure Go — no cgo.** `modernc.org/sqlite` was chosen over `mattn` for this
reason; the binary must stay statically linked so it can be dropped onto a box
with no toolchain. Don't add a dependency that pulls in cgo.

**`internal/autostart` compiles both backends everywhere.** There are no build
tags: launchd and systemd are both always built so each is testable from either
platform. Keep it that way — put platform dispatch behind `For(goos string)`
rather than in the build system.

**Unix socket paths are capped at 104 bytes on darwin.** Tests that build a
socket path under `t.TempDir()` will blow through it; use the short-directory
helper the existing tests use.

**`internal/tsnet` is tested through the `startNode` seam,** so the suite runs
with no Tailscale credentials. `Attach` is deliberately non-blocking — it starts
a background `bringUp` with retry backoff, because a node stuck in `NeedsLogin`
must not wedge the reconcile loop or the CLI.

**In `internal/tui`, measure display cells, not bytes.** Use `lipgloss.Width`,
never `len()`, on anything user-visible. This package has had three separate
bugs of exactly this shape (`pad`, `truncate`, `columnWidths`); grep for `len(`
before adding to it.

**`spec.md` belongs to the repository owner.** Read it, don't edit it.

## Conventions

`CONTRIBUTING.md` is canonical: branch from main as `<type>/<short-name>`, one
logical change per PR, full suite green before opening. Commit subjects follow
`<type>(<scope>): <imperative summary>` — see `.gitmessage`, which contributors
opt into with `git config commit.template .gitmessage`. Comments explain *why*,
not *what*, and diffs stay minimal — don't reformat unrelated lines.

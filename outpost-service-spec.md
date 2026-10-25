# Outpost Service Spec

What a service must do to be supervised by Outpost. Hand this to whoever — or
whatever — is writing the service.

Outpost does not care what your service does internally, what language it is
written in, or how it is structured. It cares about six things: how you listen,
how you report health, how you take configuration, where you put state, how you
exit, and how you log. Everything below is derived from the running code, not
from intent; where Outpost's behaviour has a number attached, that number is
what the code actually uses.

Companion documents: [`README.md`](README.md) for operating Outpost,
[`spec.md`](spec.md) for its design.

---

## 1. Listen on one TCP port, on loopback, in plain HTTP

- **One port.** The operator declares it in the manifest. Your service must
  listen on exactly that port.
- **Loopback only.** Bind `127.0.0.1`, not `0.0.0.0`. Outpost reaches you on
  `127.0.0.1:<port>`, and when Tailscale is enabled a `tsnet` node reverse-proxies
  to that same address. Binding a public interface defeats the model: nothing
  should be reachable except through Tailscale. An installation running with
  `tailscale.enabled = false` reaches services on loopback only, so the same
  rule holds.
- **Plain HTTP.** Outpost probes `http://127.0.0.1:<port><healthcheck>`. Do not
  terminate TLS yourself. Tailscale provides transport security; a service that
  only speaks HTTPS on loopback will fail every healthcheck.

### How you learn your port

Outpost does *not* tell you your port. The operator writes it twice in the
manifest — once in `port`, so Outpost knows where to probe, and once in whatever
way your service actually takes it:

```toml
binary = "/opt/outpost/bin/plumbline"
args   = ["serve", "--port", "8081"]   # how your service learns the port
port   = 8081                          # where Outpost probes
```

Outpost now catches the common case: `outpost apply` rejects a manifest whose
`port` disagrees with a `--port`/`-port` argument, because otherwise your service
runs fine while Outpost health-checks a port nothing is listening on and
restart-loops it forever.

That check is a heuristic — it only recognises the conventional spellings. If
your service takes its port some other way (`--listen`, `-p`, a config file),
nothing validates the agreement and the mismatch is yours to avoid.

**What to do about it:** accept the port from *both* a flag and an environment
variable, preferring the flag. Support `OUTPOST_PORT` specifically — it costs
three lines, and every manifest `outpost add` generates sets it, so supporting
it means the operator need not write `args` at all. Never hard-code a port, and
never pick one dynamically: Outpost has to know it in advance.

---

## 2. Serve a healthcheck

- **Path:** `/up` by default; the operator may configure another. It must begin
  with `/`.
- **Success:** any 2xx. Redirects are followed, so a 302 landing on a 200 counts
  as healthy. Anything else — 3xx that does not resolve, 4xx, 5xx, connection
  refused, timeout — is a failure.
- **Method:** `GET`. No headers are sent. No authentication is performed, so the
  healthcheck must not require any.
- **Response body is ignored.** Outpost reads and discards up to 4 KiB. Return
  whatever you like, or nothing.

### Timing you must live within

| Behaviour | Value |
|---|---|
| Probe interval | every reconcile tick, 3s by default |
| Per-probe timeout | 2s |
| Consecutive failures before restart | 3 |
| Startup grace before failures count | `start_grace_seconds`, 30s by default |

So a running service that stops answering is restarted after roughly three
consecutive failures — on the order of ten seconds. Design accordingly:

- **Keep it cheap.** It runs every 3 seconds forever. It must not query a
  database, take a global lock, or allocate meaningfully.
- **Answer within 2 seconds**, always. A healthcheck that occasionally takes
  longer is a healthcheck that occasionally restarts your service.
- **Do not report healthy before you are ready.** If you bind the port before
  you can serve real traffic, return 503 until you can. That is what the startup
  grace window is for.
- **Think carefully before reporting unhealthy for a dependency outage.** If
  your database is down and you return 500, Outpost will restart you — which
  will not fix the database, and loses whatever in-memory state you had. Prefer
  reporting healthy-but-degraded, and let the healthcheck mean "this process is
  working," not "the whole system is working."

### Starting slowly is fine

You get `start_grace_seconds` (default 30) from process start before failed
healthchecks count against you. During that window Outpost reports your status
as `starting` and leaves you alone. The window ends early at your first
successful healthcheck.

If your service genuinely needs longer — large migrations, a big index to
load — tell the operator to raise `start_grace_seconds` rather than faking
health. A crash during the grace window is still a crash and is restarted per
policy; the grace covers slow healthchecks, not a process that exits.

---

## 3. Take configuration from environment variables or flags

Outpost injects, in this order:

1. The daemon's own environment
2. Every key in the manifest's `env` table
3. With the login gateway on: `OUTPOST_AUTH=gateway`, `OUTPOST_ISSUER` and
   `OUTPOST_JWKS_URL`
4. `OUTPOST_STORAGE_DIR`

What Outpost sets comes after the manifest's `env` and therefore always wins —
a manifest cannot hand your service the wrong storage path, or claim a gateway
that isn't there, even by mistake.

There is no shared configuration file format and Outpost will never write one
for you. Read your configuration from the environment, from flags in `args`, or
from a file inside your storage directory that you manage yourself.

Do not expect a config file at a well-known path, and do not require an
interactive prompt, a TTY, or a login step at startup. Outpost starts you
unattended.

### Secrets

The manifest is a plaintext TOML file on disk. Anything in `env` is readable by
anyone who can read `~/.outpost/services/`, and appears in the process
environment. Outpost has no secrets management. If your service needs a
credential, prefer reading it from a file path handed to you by env, so the
secret itself is not in the manifest.

---

## 4. Keep all persistent state in one directory

Outpost hands you `OUTPOST_STORAGE_DIR`. It is created with mode `0700` before
your process starts, so it always exists and is writable by the time you run.

**Everything you need to survive a restart goes there.** Nothing else is
preserved. This is not merely a convention — it is the unit Outpost backs up and
restores:

- `outpost backup <service>` stops you, tars that directory, and starts you again.
- `outpost restore <service>` stops you, **replaces that directory wholesale**,
  and starts you again.

Two consequences worth designing for:

- **Your storage directory can be swapped underneath you between runs.** You
  must tolerate starting up and finding older contents than you last wrote. Do
  not cache anything about it across a restart — for instance, do not memoise a
  schema version anywhere but inside the directory itself.
- **State written anywhere else is invisible to backup and lost on restore.**
  Not `$HOME`, not `/tmp`, not next to your binary, not a system-wide path.

Symlinks inside your storage directory are preserved by backup, but a restore
refuses any archive whose entries escape the directory. Keep symlinks internal.

---

## 5. Exit non-zero on unrecoverable failure

Your exit code is how Outpost's restart policy makes decisions.

| Exit | Under `on-failure` (default) | Under `always` | Under `never` |
|---|---|---|---|
| Non-zero | Restarted with backoff | Restarted | Marked failed, left alone |
| Zero | **Not restarted** | Restarted | Not restarted |

The trap is exiting `0` on an error. Under the default policy that reads as "I
finished cleanly and meant to stop," and Outpost will leave your service down.
If you cannot continue, exit non-zero — always.

Restart backoff starts at `backoff_seconds` (default 5), doubles on each
consecutive restart, and caps at 300s. It resets once you have run healthily for
60 seconds. A service that crashes on startup every time therefore ends up being
retried every five minutes, not every five seconds.

---

## 6. Handle SIGTERM, and exit promptly

- Outpost sends **SIGTERM** to stop your service.
- You get **10 seconds**. Then it sends **SIGKILL**.
- Signals go to the **whole process group** — Outpost starts you with your own
  group, so any subprocess you spawn is signalled too.

So: trap SIGTERM, stop accepting new work, flush anything that must reach disk,
and exit. If your shutdown genuinely cannot complete in ten seconds, it will be
cut short with SIGKILL, so make the first thing you flush the thing you can
least afford to lose.

Do not ignore SIGTERM. Do not treat it as a reload signal.

You will be stopped and started frequently and at arbitrary moments — on
restart, on backup, on restore, on manifest change, when the daemon shuts down.
Restarts must be routine and non-destructive, never something requiring repair.

---

## 7. Log to stdout and stderr

Outpost captures both streams into `~/.outpost/logs/<service>.log`, rotating at
10 MiB and keeping 3 previous files.

- **Write to stdout/stderr, not to your own log file.** A file you manage
  yourself is not captured, not rotated, and not visible to `outpost logs`.
- **Do not buffer heavily.** A log line that sits in a buffer for a minute is
  useless while diagnosing a crash loop. Line-buffered or unbuffered.
- **No ANSI colour, no progress bars, no cursor control.** The destination is a
  file, not a terminal. Detect that you are not on a TTY, or simply never emit
  escape sequences.
- Structured output (one JSON object per line) is a good default, but Outpost
  does not parse your logs and does not care either way.

---

## 8. Do not daemonize

Outpost supervises the process it starts. It tracks that PID, waits on it, and
uses its exit status.

Therefore your service must **run in the foreground until it is told to stop**.
Specifically, do not:

- fork into the background, double-fork, or call `daemon(3)`
- write a pidfile and exit
- exec a supervisor, wrapper, or process manager of your own
- re-exec yourself under a different PID

If your process exits, Outpost concludes the service stopped — and if it exited
zero, that it stopped deliberately.

---

## 9. Optional, and forward-looking

None of this is required to be supervised. It matters for how a service is
*presented* once the remaining Outpost features land.

**MCP endpoint.** If you serve MCP, do it at `/mcp` and have the operator set
`mcp = true`. That flag is informational today — Outpost records it and does
nothing else with it.

**Machine-readable API description.** Serving your OpenAPI document at
`GET /v1/openapi.json` is what Outpost's Hermes `SKILL.md` generator will
consume when it is built (`spec.md` §11). Providing it now costs nothing and
means your service needs no hand-written skill content later.

**REST conventions.** The parent personal-utils framework puts REST under
`/v1/*`, MCP at `/mcp`, and health at `/up`. A service following that shape
satisfies this document automatically and will need no per-service configuration
when Outpost gains the ability to add services from a repository URL.

---

## 10. What Outpost does not give you

Do not build expecting any of this:

| Not provided | What to do instead |
|---|---|
| TLS | Tailscale terminates it; serve plain HTTP on loopback |
| A bind address | You bind loopback; Outpost never binds your port |
| Authentication or authorization | Tailnet membership is the boundary; add your own if you need more. Behind the login gateway, the user arrives in `X-Outpost-*` headers; trust them only when `OUTPOST_AUTH=gateway` |
| Secrets management | Read secrets from a file path given via env |
| Service discovery | No registry; addresses are the operator's business |
| A database, cache, or queue | Bring your own, inside your storage directory |
| Scheduling or cron | Not yet decided (`spec.md` open question 2) |
| Multiple instances, scaling, load balancing | Exactly one process per declared service |
| Config file management | Read env and flags |
| Log parsing, metrics, tracing | Logs are captured verbatim and nothing more |

Outpost supervises **native processes only**. There is no container runtime and
none is planned; `runtime` accepts only `"process"`.

---

## 11. Conformance checklist

A service conforms when all of these are true.

**Networking**
- [ ] Listens on exactly the port the operator declares
- [ ] Binds `127.0.0.1`, not `0.0.0.0` or a public interface
- [ ] Speaks plain HTTP, not HTTPS
- [ ] Accepts its port from a flag and from `OUTPOST_PORT`
- [ ] Never selects a port dynamically

**Health**
- [ ] Serves `GET /up` (or the configured path) returning 2xx when healthy
- [ ] Healthcheck needs no authentication
- [ ] Responds in well under 2 seconds, every time
- [ ] Cheap enough to run every 3 seconds indefinitely
- [ ] Returns non-2xx until genuinely ready to serve
- [ ] Does not report unhealthy merely because a dependency is down

**Configuration**
- [ ] Reads all configuration from environment variables and flags
- [ ] Starts unattended: no prompts, no TTY, no interactive login
- [ ] Requires no config file at a well-known path

**State**
- [ ] Writes all persistent state under `OUTPOST_STORAGE_DIR`
- [ ] Writes no persistent state anywhere else
- [ ] Starts correctly when that directory has been replaced by a restore
- [ ] Any symlinks it creates stay inside that directory

**Lifecycle**
- [ ] Exits non-zero on unrecoverable failure
- [ ] Never exits zero on an error
- [ ] Handles SIGTERM: stops work, flushes, exits
- [ ] Completes shutdown within 10 seconds
- [ ] Runs in the foreground; never daemonizes or writes a pidfile
- [ ] Restarts cleanly and repeatedly with no manual repair

**Logging**
- [ ] Logs to stdout and stderr only
- [ ] Does not buffer log output for long periods
- [ ] Emits no ANSI escape sequences

---

## 12. Reference implementation

A conforming service, complete, in Go. Everything Outpost requires is here and
nothing else is.

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	// Accept the port from a flag, falling back to OUTPOST_PORT.
	port := flag.Int("port", envInt("OUTPOST_PORT", 0), "TCP port to listen on")
	flag.Parse()
	if *port == 0 {
		log.Fatal("no port: pass --port or set OUTPOST_PORT")
	}

	// Storage is created 0700 before we start, so it always exists.
	storage := os.Getenv("OUTPOST_STORAGE_DIR")
	if storage == "" {
		log.Fatal("OUTPOST_STORAGE_DIR is not set")
	}

	// Unbuffered, no colour: this goes straight to a log file.
	log.SetFlags(log.LstdFlags | log.LUTC)

	var ready atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("GET /up", func(w http.ResponseWriter, r *http.Request) {
		// Cheap and honest: no I/O, no locks. Not ready means not ready.
		if !ready.Load() {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1/hello", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "hello")
	})

	// Loopback only: Tailscale is the only way in.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		log.Fatalf("listen: %v", err) // non-zero exit: Outpost will restart us
	}
	srv := &http.Server{Handler: mux}

	go func() {
		if err := initialise(storage); err != nil {
			log.Fatalf("initialise: %v", err) // non-zero exit
		}
		ready.Store(true) // ends the startup grace window
		log.Printf("ready on 127.0.0.1:%d, storage=%s", *port, storage)
	}()

	// SIGTERM is a stop request. We have 10 seconds before SIGKILL.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err) // non-zero exit
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")

	// Leave headroom inside Outpost's 10s grace.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Println("stopped")
}

// initialise does the slow startup work: migrations, indexes, warmup.
func initialise(storage string) error {
	// All persistent state lives under storage, and nowhere else.
	return os.MkdirAll(filepath.Join(storage, "db"), 0o700)
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return fallback
}
```

Its manifest:

```toml
# ~/.outpost/services/hello.toml
binary  = "/opt/outpost/bin/hello"
args    = ["--port", "8090"]
port    = 8090                       # must match the port in args
storage = "~/.outpost/data/hello"
tailscale_name = "hello"
```

---

## 13. Verifying conformance

Check these by hand before handing a service to Outpost. Each maps to a rule
above and each has caught a real problem.

```sh
# 1. Binds loopback only — expect 127.0.0.1, never 0.0.0.0 or *
./myservice --port 8099 &
lsof -nP -iTCP:8099 -sTCP:LISTEN

# 2. Healthcheck answers, in plain HTTP, quickly, with no auth
curl -sS -o /dev/null -w '%{http_code} in %{time_total}s\n' http://127.0.0.1:8099/up

# 3. Reports not-ready before it is ready — run immediately after starting
#    Expect a non-2xx first, then 200

# 4. Handles SIGTERM and exits within 10 seconds
time kill -TERM %1        # expect a clean exit well inside 10s

# 5. Exits non-zero on failure — expect a non-zero code
./myservice --port 1 ; echo "exit=$?"

# 6. Writes nothing outside its storage directory
OUTPOST_STORAGE_DIR=/tmp/probe ./myservice --port 8099 &
# exercise it, stop it, then confirm state landed only in /tmp/probe

# 7. Survives its storage being replaced
rm -rf /tmp/probe && mkdir -p /tmp/probe   # restore, in effect
OUTPOST_STORAGE_DIR=/tmp/probe ./myservice --port 8099   # must start clean

# 8. Does not daemonize — the PID you start must be the one that stays
./myservice --port 8099 & echo "started $!" ; sleep 2 ; ps -p $!
```

Then let Outpost exercise it for real:

```sh
outpost apply
outpost serve &
outpost list                 # expect: running
outpost restart myservice    # expect: a new instance id, still running
outpost backup myservice
outpost restore myservice    # expect: still running afterwards
outpost logs myservice       # expect: your stdout and stderr
```

`internal/runtime/testdata/fakeservice` in this repository is a minimal
conforming service that can be told to crash, stall its startup, or go unhealthy
on demand. It is useful both as a worked example and as something to test
Outpost's own behaviour against.

## 14. Publishing releases `outpost add` can install

Everything above is about running correctly once Outpost starts you. This
section is about the step before that: making

```sh
outpost add https://github.com/you/yourservice
```

enough to install and configure your service, instead of the operator building
it first and hand-writing a manifest.

None of this is required. A service that satisfies §1–§8 works perfectly well
when added with `--binary`. This only removes a manual step.

### Publish a release with per-platform binaries

`outpost add` reads your repository's **latest non-prerelease GitHub release**
and installs one asset from it. It never builds from source: running a
repository's build scripts is a much larger trust decision than downloading a
binary its author published, and Outpost does not make it on the operator's
behalf.

Name the asset so it contains the platform:

```
yourservice-linux-amd64
yourservice-linux-arm64
yourservice-darwin-arm64
```

Go's `GOOS`/`GOARCH` spellings are preferred, but the common alternatives are
understood too — `macos`, `osx`, `apple` and `mac` for darwin, `x86_64`,
`x86-64` and `x64` for amd64, `aarch64` for arm64, `i386`, `i686` and `x86`
for 386, `armv7`/`armv6`/`armhf` for arm, `win` for windows. They match as
whole words between `-`, `_` and `.`, so `hmac-proxy` does not read as a mac
build.

**Archives are unpacked.** A `.tar.gz` or `.zip` is fine — Outpost takes the
binary out of it, choosing the file named after the service (or after the
program the asset is named for, so `--name` does not hide it), or the only
executable if there is exactly one. A bare binary is preferred when you publish
both. When Outpost has to guess, it only takes a binary-shaped name or one of
those two archives. A binary-shaped name has no extension: a final dot segment
that is a platform word (`svc.linux.amd64`) or a version (`svc-1.0.0rc1`)
counts as part of the name. `.bin` is also accepted, as is `.exe` on Windows
and `.AppImage` on Linux. `.tar.xz`, `.gz`, `.7z`, `.deb`, `.apk`, checksums
and signatures are never taken for the binary. An asset named by `asset` in
the descriptor is taken as named unless it is one of those.

If your release ships variants for the same platform — a `_gui` build beside
the plain one — Outpost takes the plainest name, and says which it passed over.
That only applies when one name is a prefix of the other, so two genuinely
different programs sharing a platform are still an error rather than a guess.

### Publish checksums

If your release includes a `SHA256SUMS` file (or `checksums.txt`, or a
`<asset>.sha256`), Outpost verifies the download against it and **refuses to
install on a mismatch**. If your release publishes none, Outpost installs
anyway and warns that it could not verify.

The format is `sha256sum` output — `<hex>  <filename>`, one per line. Generating
it is one line in a release workflow:

```sh
sha256sum yourservice-* > SHA256SUMS
```

### Describe yourself in `outpost.toml`

Put an `outpost.toml` in your repository root — or attach it to the release,
which is preferred, because it is then the copy that shipped with that binary:

```toml
# outpost.toml
name        = "yourservice"                    # default: the repository name
args        = ["serve", "--port", "${PORT}"]   # ${PORT} is substituted
env         = { LOG_LEVEL = "info" }           # ${PORT} works here too
healthcheck = "/up"                            # default: /up
mcp         = true                             # you serve /mcp
restart_policy = "on-failure"                  # on-failure | always | never
start_grace_seconds = 15                       # if you are slow to bind
asset       = "yourservice-${GOOS}-${GOARCH}"  # only if your names are unusual
```

**`${PORT}` is the important one.** Outpost assigns the port — that is how it
keeps services from colliding (§1) — so a service that takes its port as a flag
has no other way to be told what it got.

If you read the port from `OUTPOST_PORT` instead (§1), you need no `args` at
all: `outpost add` writes `OUTPOST_PORT` into every manifest it generates. Note
that this is a property of the generated manifest, not of the runtime — Outpost
itself only ever injects `OUTPOST_STORAGE_DIR` (§4), so a hand-written manifest
gets `OUTPOST_PORT` only if it says so.

Setting a fixed `port` is possible and usually wrong: it removes Outpost's
ability to avoid a collision with another service on the same machine.

Every field is optional, and an operator's explicit flag always wins over what
you declare here — `--healthcheck /ready` overrides your `healthcheck`. What
you put here are defaults, not requirements.

### What this does not do

Upgrades are not handled: `outpost add` on a service that already exists
refuses unless given `--force`. Private repositories work if the operator has
the `gh` CLI authenticated, or `GITHUB_TOKEN` set; Outpost stores no
credentials of its own.

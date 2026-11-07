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
  `127.0.0.1:<port>`, and when Tailscale is enabled your service gets its own
  `tsnet` node, `<tailscale_name>.<tailnet>.ts.net`, which reverse-proxies to
  that same address. With the login gateway on as well, that node serves HTTPS
  and passes every request through the gateway's login before it reaches you
  (§3). Binding a public interface defeats the model: nothing should be
  reachable except through Tailscale and the gateway. An installation running
  with `tailscale.enabled = false` reaches services on loopback only, so the
  same rule holds.
- **Plain HTTP.** Outpost probes `http://127.0.0.1:<port><healthcheck>`. Do not
  terminate TLS yourself. Your Tailscale node terminates it, with a certificate
  for its own name; a service that only speaks HTTPS on loopback will fail
  every healthcheck.

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

### Protocols that aren't HTTP

Your HTTP port is the one Outpost probes and the gateway fronts. If you also
speak something else — SSH, a database wire protocol, `git://` — the operator
can have your tailnet node carry it as raw TCP:

```toml
port = 8081                  # HTTP: healthcheck, gateway, as above
tcp  = { 22 = 2222 }         # tailnet port = your port on 127.0.0.1
```

The same rules hold for these ports: bind `127.0.0.1`, take each port from a
flag or the environment, never pick one dynamically. Outpost doesn't tell you
which ports it forwards; the operator writes them twice, as for `port`.

What changes is what reaches you (§3). The gateway can't log a byte stream
in, so with it on the node asks it about each connection instead and lets in
only devices whose Tailscale login belongs to an Outpost user allowed to open
your app. You don't learn which user: every connection arrives from
`127.0.0.1`, with nothing added to it. So:

- **Authenticate on these ports yourself.** The gate narrows who can reach
  you to people the operator has given an account; it doesn't say which of
  them is calling. Your SSH keys, passwords or tokens decide what each may do.
- **Don't trust the source address.** It is always loopback; per-address
  limits and logs see one client.
- **Never forward to your HTTP port.** It would reach you without the
  gateway's login or identity headers; `outpost apply` refuses it.

With the gateway off, any device on the tailnet can connect, as for HTTP.

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
3. Every secret bound to the service (see [Secrets](#secrets)), replacing a
   manifest `env` key of the same name
4. `OUTPOST_AI_URL` and `OUTPOST_AI_TOKEN`, for the operator's models (see
   [AI models](#ai-models))
5. With the login gateway on: `OUTPOST_AUTH=gateway`, `OUTPOST_ISSUER` and
   `OUTPOST_JWKS_URL`
6. `OUTPOST_STORAGE_DIR`

What Outpost sets comes after the manifest's `env` and therefore always wins —
a manifest cannot hand your service the wrong storage path, or claim a gateway
that isn't there, even by mistake.

There is no shared configuration file format and Outpost will never write one
for you. Read your configuration from the environment, from flags in `args`, or
from a file inside your storage directory that you manage yourself.

Do not expect a config file at a well-known path, and do not require an
interactive prompt, a TTY, or a login step at startup. Outpost starts you
unattended.

### Behind the login gateway

When `OUTPOST_AUTH=gateway` is set, Outpost has already authenticated every
request you receive, on every path, except its own healthcheck on loopback.
This is how an operator gets one login for all their services, so:

- **Present no login of your own.** Treat the request as signed in. If your
  service has optional built-in authentication — a password flag, a users
  table — leave it off when `OUTPOST_AUTH=gateway`, or the operator signs in
  twice.
- **Learn who is calling from the headers.** Every request carries:

  | Header | Contents |
  |---|---|
  | `X-Outpost-User` | the username |
  | `X-Outpost-Name` | display name; may be empty |
  | `X-Outpost-Email` | may be empty |
  | `X-Outpost-Groups` | comma-separated; may be empty |
  | `X-Outpost-Identity` | an Ed25519-signed JWT (`alg` `EdDSA`) |

  The gateway removes any `X-Outpost-*` header a client sends before setting
  its own, so nothing that came through the gateway can forge them. But your
  port is on loopback, and the gateway is not the only thing on the machine:
  any other process there — another service Outpost runs, for one — can
  connect to your port directly and send whatever `X-Outpost-User` it likes.
  The plain headers are fine for knowing who is using you; for anything a
  neighbour on the machine must not be able to do as someone else, verify
  `X-Outpost-Identity` against the keys at `OUTPOST_JWKS_URL`: `iss` is
  `OUTPOST_ISSUER`, `aud` is the host you are being served at, `sub` is the
  username, and it also carries `name`, `email` and `groups`. It lives for 60
  seconds, so verify per request rather than keeping it. Trust none of these
  headers when `OUTPOST_AUTH` is unset — nothing strips them then.
- **Check the origin of your own forms.** Every app behind one gateway is on
  the same site, `<name>.<domain>`, so the browser sends the user's cookie
  with a request one app's page makes to another, and the gateway adds the
  user's identity to it. The gateway checks `Origin` on its own forms only.
  For anything of yours that changes state, require `Origin` (or `Referer`)
  to be your own host, or use a CSRF token.
- **Hard-code no hostname.** You are served at your node's own tailnet address,
  `https://<tailscale_name>.<tailnet>.ts.net` — or, if Tailscale had to rename
  the node because the name was taken, at the name it was given (`yarr-1`).
  Without Tailscale, it is `<name>.<domain>` from the gateway's configuration.
  `Host` is preserved and `X-Forwarded-Host` and `X-Forwarded-Proto` are set,
  so build absolute URLs from those, or use relative ones.
- **Leave `/.outpost/` alone.** Paths under it, on your host, belong to the
  gateway — the sign-in callback and sign-out — and are never passed to you.
  To offer sign-out, `POST` a same-origin form to `/.outpost/logout`; it ends
  the user's whole Outpost login, not only yours.
- **Leave the gateway's cookie alone.** It is called `__Host-outpost` (or
  `outpost` on a plain-http gateway) and is removed before the request reaches
  you. Don't name a cookie of your own either of those.

None of this covers a raw TCP port (§1): no headers reach you there, and
`OUTPOST_AUTH=gateway` says nothing about who opened the connection. Treat
those ports as authenticated by you alone.

Non-browser clients sign in with a personal access token instead
(`outpost token create`), sent as `Authorization: Bearer opat_...` or in
`Outpost-Token`. A request carrying one reaches you exactly as a signed-in
browser's does, with the same identity headers and identity token, and the
token itself is removed first. A bearer token without the `opat_` prefix is
left alone, so your own API keys still reach you in `Authorization`. With no
credential at all, an unauthenticated `GET` is redirected to the sign-in page
and anything else is refused with `401`.

#### Public paths and webhooks

The operator can let some of your paths skip the login, with `public_paths` in
your manifest or in your app's Internet access setting in the portal: webhooks and feeds that
carry credentials of their own. With `funnel` on as well, those paths, and
only those, are answered from the **public internet** through Tailscale
Funnel, at your own tailnet address. You can suggest paths in your
`outpost.toml` (§14), but whether they are public, and whether the internet
reaches them, is the operator's call. Plan for both.

- **A request on a public path has no identity.** No `X-Outpost-*` header and
  no Outpost cookie reaches you, and Outpost access tokens are refused before
  you see them. Treat it as anonymous, whoever it claims to be.
- **Assume it may be from anyone on the internet.** It may arrive any number
  of times, from scanners as well as the sender you expect. Outpost makes sure
  only the paths you were given are reachable, with the body intact, but it
  can't tell a real webhook from a forged one.
- **Verify every webhook's signature**, such as GitHub's `X-Hub-Signature-256`
  or Stripe's `Stripe-Signature`, against a secret Outpost hands you as an
  environment variable (see Secrets below), and reject anything that doesn't
  match. Keep the handler idempotent, since senders retry.
- **Stay within the limits.** Bodies over 25 MiB are refused, upgrades such as
  WebSockets are refused, and a request gets about a minute. Answer quickly
  and do slow work afterwards.
- **Expect `X-Forwarded-Proto: https`** and the sender's address in
  `X-Forwarded-For`.

### Secrets

The manifest is a plaintext TOML file on disk. Anything in `env` is readable by
anyone who can read `~/.outpost/services/`. Put a credential in an Outpost
secret instead (`outpost secret set`, or your app's Secrets under its Manage menu in the portal) and it
reaches you as an ordinary environment variable, under whatever name you
document, without being in the manifest. The operator can give a secret to your
app alone, or share one with every app; when both set the same variable, the
one given to your app wins.

Read it once at startup. When the operator replaces a secret's value, Outpost restarts
you with the new value rather than changing anything under a running process,
so you don't need to watch for it; you do need to start cleanly when that
happens, which section 6 already asks. Like anything else in your environment,
don't log it and don't echo it back in a response.

### AI models

Every service is also started with `OUTPOST_AI_URL` and `OUTPOST_AI_TOKEN`,
which reach the two models the operator chose for the whole installation: an
LLM and a System 1 model. You don't need them. If you want them, §15 is how
to call them and what to expect back.

---

## 4. Keep all persistent state in one directory

Outpost hands you `OUTPOST_STORAGE_DIR`. It is created with mode `0700` before
your process starts, so it always exists and is writable by the time you run.
It is also your working directory, so a relative path lands inside it.

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

**MCP endpoint.** If you serve MCP, use the Streamable HTTP transport at a
single path, `/mcp` by convention, and say where in the manifest:

```toml
mcp_path = "/mcp"
```

With the gateway on, an MCP client holding an access token then has two ways
in, both behind the gateway:

- `https://<tailscale_name>.<tailnet>.ts.net<mcp_path>`, your own address, as
  for any other request.
- `https://<portal>/mcp/<name>`, one address on the portal for every service's
  MCP. The gateway sends it to your `mcp_path`, keeping the query string.

Requests that come in through the portal arrive as if they had come to your own
address: `Host` and `X-Forwarded-Host` are your own host, not the portal's, and
the identity token's `aud` is your host too, so verifying it needs nothing
extra. They only ever carry an access token's identity. `Set-Cookie` is dropped
from your responses to them, because they are on the portal's origin; keep MCP
session state in `Mcp-Session-Id`, which passes through both ways. A response
can stream (`text/event-stream`), and there is no time limit on how long you
take to answer. Only the one path is routed, so the old SSE transport, which
hands the client a second path, does not work through the portal.

`mcp = true` in your descriptor means you serve MCP at `/mcp`: `outpost add`
writes `mcp_path = "/mcp"` into the manifest for you, which is what the portal
routes by. If you serve it somewhere else, the operator changes `mcp_path`.

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
| TLS | Your Tailscale node terminates it, with a certificate for its own name; serve plain HTTP on loopback |
| Identity on raw TCP | The gateway admits only allowed users' devices to a `tcp` forward, but tells you nothing about them; authenticate on those ports yourself |
| A bind address | You bind loopback; Outpost never binds your port |
| Webhook verification | Funnel can put your public paths on the internet, but Outpost can't check a sender's signature; verify it yourself (§3) |
| Authorization | With the login gateway on, Outpost authenticates every request and tells you who it is (§3), but every user can open every app; decide what each may do yourself. Without the gateway, tailnet membership is the boundary |
| Secrets in a file, or rotated in place | Outpost hands secrets over as environment variables and restarts you to change one; if you need a credential as a file, write it into your storage directory yourself |
| A choice of AI model | Outpost gives you one LLM and one System 1 model, chosen by the operator from Ollama or a hosted provider (§15); for a model of your own choosing, bring its provider and key as a secret |
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
- [ ] Any other protocol it speaks is on its own loopback port, declared in `tcp`, never the HTTP port

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

**Behind the gateway** (when `OUTPOST_AUTH=gateway`)
- [ ] Presents no login of its own
- [ ] Takes the user from `X-Outpost-User`, or from the verified `X-Outpost-Identity`
- [ ] Trusts `X-Outpost-*` headers only when `OUTPOST_AUTH=gateway`
- [ ] Verifies `X-Outpost-Identity` for anything another process on the machine must not be able to do
- [ ] Checks `Origin` on its own state-changing requests
- [ ] Hard-codes no hostname; uses relative URLs, or `Host` and `X-Forwarded-*`
- [ ] Serves nothing under `/.outpost/` and sets no cookie named `outpost` or `__Host-outpost`
- [ ] Treats requests on public paths as anonymous, and verifies each webhook's signature
- [ ] Authenticates every connection on a `tcp` port itself, and doesn't trust its source address

**Updates** (when installed from a release, §14)
- [ ] Every release has its binaries attached by the time it is published
- [ ] Asset names keep the same shape from one release to the next
- [ ] Data migrations finish before the healthcheck passes, and a failed one exits non-zero
- [ ] A new release still accepts the args and env an earlier `outpost.toml` declared

**AI models** (if it uses them, §15)
- [ ] Reads `OUTPOST_AI_URL` and `OUTPOST_AI_TOKEN` from the environment, and treats AI as off when they're unset
- [ ] Sends `llm` or `system1` as the model, never a provider's model name
- [ ] Keeps running, and stays healthy, when a model is unavailable or not given to it (503, 502, 403)
- [ ] Sticks to what every provider takes: the OpenAI chat API if it can, and System 1 questions with instructions, two or more labels, and two to ten levels
- [ ] Allows minutes for a first request, while Ollama loads the model
- [ ] Doesn't log the token, or send it anywhere but `OUTPOST_AI_URL`

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

	// Loopback only: Tailscale, through the gateway, is the only way in.
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
that this is a property of the generated manifest, not of the runtime — what
Outpost itself injects is listed in §3, and `OUTPOST_PORT` isn't among it, so
a hand-written manifest gets `OUTPOST_PORT` only if it says so.

Setting a fixed `port` is possible and usually wrong: it removes Outpost's
ability to avoid a collision with another service on the same machine.

Every field is optional, and an operator's explicit flag always wins over what
you declare here — `--healthcheck /ready` overrides your `healthcheck`. What
you put here are defaults, not requirements.

### Updates

`outpost add` records your repository and the release it installed, and
`outpost update <service>` later installs a newer release the same way: it
picks your asset by the same rules, verifies it against the same checksums,
replaces the binary, restarts you, and puts the old binary back if you do not
pass your healthcheck. Four things follow for you:

- **Attach the binaries before the release is published.** Update takes the
  latest release, and one with nothing attached yet is refused until it has
  something. Create it as a draft and publish once the uploads are done, or
  upload in the same workflow step that creates it.
- **Keep asset names the same shape.** Update chooses your asset by the same
  rules as `add`, so a renaming those rules cannot place breaks every
  operator's next update.
- **Migrate data before you report healthy, and exit non-zero if it fails.**
  The operator's storage is backed up just before the update, and a release
  that never passes its healthcheck is rolled back. But only the binary goes
  back, so a migration that succeeds and then leaves you unhealthy hands the
  previous release data it may not read. Fail before you change anything if
  you can.
- **Stay compatible with your old `outpost.toml`.** An update replaces only the
  binary; the manifest keeps the args and env your descriptor declared when the
  service was first added. Changing them in a later release reaches new
  installs, not existing ones, so keep accepting the old ones (or say in your
  release notes what operators should edit).

### What this does not do

Adding a service that already exists refuses unless given `--force`; to move
an installed service to a newer release, use `outpost update`. Private
repositories work if the operator has the `gh` CLI authenticated, or
`GITHUB_TOKEN` set; Outpost stores no credentials of its own.

## 15. Using the operator's AI models

Outpost can give your service a model to call without you choosing one,
running one, or holding a key. The operator picks two for the whole
installation, each from [Ollama](https://ollama.com) on their machine or from
a hosted provider (OpenAI, Anthropic, OpenRouter, TypeSafe):

- **The LLM** generates text: chat, summaries, extraction, tool calls, as far
  as the model chosen supports them. Gemma 4, Claude Haiku or a GPT model, for
  instance.
- **The System 1 model** writes no text. You give it an input and typed
  questions, and in one fast pass it answers each with probabilities: which
  label fits, yes or no, where on a scale. Laya, Jev or OpenAI's
  `gpt-6-luna`, for instance. Use it for routing, triage, moderation and
  scoring, where an LLM would be slow and its answer would need parsing.

Which provider is behind each is the operator's business, and can change
while you run. You call Outpost the same way whichever it is, and never hold
a provider's key: Outpost adds it to each request.

The operator may also give each kind to only some apps, since hosted models
cost money. If yours isn't one of them, its requests for that kind get a 403
(see What can go wrong).

Everything here is optional. A service that never reads these variables is
unaffected.

### What you're given

Every service is started with two variables (§3):

```
OUTPOST_AI_URL=http://127.0.0.1:47901
OUTPOST_AI_TOKEN=oai_…
```

- **`OUTPOST_AI_URL`** is a loopback endpoint the Outpost daemon serves. The
  port is the operator's choice, so read it from the variable rather than
  assuming `47901`. It's plain HTTP and only this machine can reach it.
- **`OUTPOST_AI_TOKEN`** is yours alone. Every request needs it, as
  `Authorization: Bearer <token>` or as `x-api-key: <token>`, whichever your
  SDK sends. It stays the same across restarts of you and of the daemon, and
  changes only when the operator replaces it, which restarts you with the new
  one. Read it from the environment at startup, every time. Treat it as a
  credential: don't log it, and don't send it anywhere but `OUTPOST_AI_URL`.

**When the variables are unset,** you aren't running under an Outpost that
has AI models: an older one, or none, such as on a developer's machine. Treat
AI as off. If you want to develop against a model without Outpost, take the
base URL and key from your own flags as well, with Outpost's variables as the
default.

### The endpoints

Pick whichever API your code already speaks. Each sits under its own prefix,
so an SDK's base URL can point there unchanged:

| API | Method and path | Reaches | Model to send |
|---|---|---|---|
| OpenAI Chat Completions | `POST /openai/v1/chat/completions` | the LLM | `llm` |
| Anthropic Messages | `POST /anthropic/v1/messages` | the LLM | `llm` |
| System 1 | `POST /systemone/v1/systemone` | the System 1 model | `system1` |
| Model list | `GET /openai/v1/models` | what `llm` and `system1` are now, for you | — |

For SDKs:

| SDK | Base URL | API key |
|---|---|---|
| OpenAI, any language | `$OUTPOST_AI_URL/openai/v1` | `$OUTPOST_AI_TOKEN` |
| Anthropic, any language | `$OUTPOST_AI_URL/anthropic` | `$OUTPOST_AI_TOKEN` |

Nothing else is served. Ollama's and the providers' other APIs, embeddings,
images and audio aren't reachable through Outpost, and their paths return
404.

The model list has an entry for each kind you may use: its `id` is `llm` or
`system1`, `outpost_model` is the model behind it now, and
`outpost_provider` is who serves it (`ollama`, `openai`, `anthropic`,
`openrouter` or `typesafe`). A kind you aren't given isn't listed.

**Either chat API reaches the LLM, with one exception.** Ollama, Anthropic and
OpenRouter take both. OpenAI doesn't take the Anthropic Messages API, so
while the LLM is an OpenAI model, `/anthropic/v1/messages` answers 400 with a
message saying to use `/openai/v1/chat/completions`. If you can, speak the
OpenAI Chat Completions API, which every LLM provider takes; if you speak
Anthropic's, handle that 400 by telling the operator.

**The path decides the model, not you.** Outpost replaces the `model` in
your request body with the model the operator has chosen for that kind, as it
is at that moment. Send `llm` or `system1` so your code reads correctly. The
operator can change the model at any time, without restarting you, so:

- don't hard-code a provider's model name, which would be replaced anyway;
- don't cache which model is behind `llm`. The reply's `model` field says
  which one answered that request, which is worth logging.

Everything else in your body passes through to the provider untouched:
messages, `system`, `temperature`, `max_tokens`, `tools`, `stream` and so on.
What a field means, and which are accepted, is the provider's, so keep to
what the API you speak documents. Outpost forwards `Content-Type`, `Accept`,
`Anthropic-Version` and `Anthropic-Beta`, and no other request header; it
adds the provider's own key, and your token is never forwarded. Replies come
back with the provider's status, body, `Content-Type`, `Cache-Control` and
`Retry-After`.

### Calling the LLM

With the OpenAI SDK, in Python:

```python
import os
from openai import OpenAI

ai = OpenAI(base_url=os.environ["OUTPOST_AI_URL"] + "/openai/v1",
            api_key=os.environ["OUTPOST_AI_TOKEN"])
reply = ai.chat.completions.create(
    model="llm",
    messages=[{"role": "user", "content": "Summarise this recipe in one line: …"}],
)
print(reply.choices[0].message.content)
```

With the Anthropic SDK:

```python
import os
from anthropic import Anthropic

ai = Anthropic(base_url=os.environ["OUTPOST_AI_URL"] + "/anthropic",
               api_key=os.environ["OUTPOST_AI_TOKEN"])
reply = ai.messages.create(
    model="llm", max_tokens=512,
    messages=[{"role": "user", "content": "Summarise this recipe in one line: …"}],
)
print(reply.content[0].text)
```

Or with no SDK:

```sh
curl -s "$OUTPOST_AI_URL/openai/v1/chat/completions" \
  -H "Authorization: Bearer $OUTPOST_AI_TOKEN" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "Say hi"}]}'
```

**Streaming** works as each API defines it (`"stream": true`): server-sent
events, passed on as the provider sends them.

**What the LLM can do depends on the model the operator chose.** Tool calls,
images and "thinking" are features of some models and not others, and
`/openai/v1/models` doesn't say which. If you rely on one, handle the
provider's refusal by falling back to a plain prompt. It comes back as the
provider's own error: a 400 from most hosted providers, currently a 500 from
Ollama, with a message saying what isn't supported, such as `image input is
not supported`. Document what you need, so the operator can choose a model
that has it.

### Calling the System 1 model

`POST $OUTPOST_AI_URL/systemone/v1/systemone` takes a `state` and
`questions`, each keyed by an ID you choose. How many questions a request may
hold depends on the model: Laya takes 64, Jev 128 and OpenAI's 400.

```json
{
  "model": "system1",
  "state": "My order arrived broken and nobody answers my emails.",
  "questions": {
    "upset": {
      "type": "noul",
      "instructions": "Is the customer upset?"
    },
    "topic": {
      "type": "choice",
      "instructions": "What is the message about?",
      "criteria": {
        "shipping": "delivery, damage or a missing parcel",
        "billing": "charges, refunds or invoices",
        "other": "anything else"
      }
    },
    "urgency": {
      "type": "score",
      "instructions": "How urgently does this need a reply?",
      "criteria": ["can wait", "this week", "today", "right now"]
    }
  }
}
```

- **`state`** is what's being judged: a string, or a JSON object when you
  have fields. Laya reads text, up to about 8,000 tokens.
- **`noul`** is a yes or no question. It takes only `instructions`.
- **`choice`** picks one label. `criteria` maps each label to a description of
  when it applies.
- **`score`** places the state on an ordered scale. `criteria` lists the
  levels, lowest first.
- **`box`** locates something in an image, for models that take images: Jev
  does, Laya doesn't, and other providers refuse it.

Write questions every provider can take, and the operator can switch freely:
give a `choice` at least two labels and a `score` two to ten levels, and give
every question `instructions`. OpenAI's decision model needs all three, and
Outpost refuses with a 400 what it can't put to it. Images are where
providers differ most: TypeSafe takes them as `assets`, OpenAI as data URLs in
`images`, and Laya not at all, so treat them as optional.

The answers come back under the same IDs:

```json
{
  "model": "laya:latest",
  "answers": {
    "upset":   {"type": "noul", "noul": 0.87},
    "topic":   {"type": "choice", "choice": "shipping", "confidence": 0.76,
                "probabilities": {"shipping": 0.94, "billing": 0.03, "other": 0.03}},
    "urgency": {"type": "score", "score": 2.89, "confidence": 0.76,
                "legend": {"0": "can wait", "1": "this week", "2": "today", "3": "right now"},
                "probabilities": {"0": 0.01, "1": 0.02, "2": 0.05, "3": 0.92}}
  },
  "usage": {"input_tokens": 137, "output_tokens": 0}
}
```

- **`noul`** is the probability of yes, from 0 to 1.
- **`choice`** is the most likely label, with every label's probability.
- **`score`** is a number on the scale, not a label: the levels are numbered
  from 0 in the order you gave them, `legend` maps each number back to its
  level, and `probabilities` are keyed by that number. 2.89 here sits between
  "today" (2) and "right now" (3), and nearer the second. Round it, or compare
  it with a threshold, as suits you.
- **`confidence`**, where given, is how sure the model is of the whole
  answer, from 0 to 1.
- **`refusal`** is a `type` some hosted models answer with instead, when they
  won't answer that question. It has nothing else in it; treat it as "don't
  know".

The shape is the same whichever provider answered. For OpenAI's decision
model, which has its own API, Outpost translates both ways.

Decide your own thresholds, and treat answers near the middle as "don't know"
rather than forcing a yes or no. Ask every question you have about one input
in a single request: they're answered in the same pass, so a second question
costs almost nothing.

### What can go wrong

Errors that Outpost itself returns have this shape, which both SDKs read a
message from:

```json
{"type": "error", "error": {"type": "api_error", "message": "no LLM is configured in Outpost; …"}}
```

| Status | Means | What to do |
|---|---|---|
| 401 | missing or wrong token | Check you send `OUTPOST_AI_TOKEN`; it isn't a request to retry |
| 403 | the operator hasn't given your service this kind | Do without it; the message says so, for your log |
| 404 | a path Outpost doesn't serve | Use one from the table above |
| 405 | wrong method | `POST`, or `GET` for the model list |
| 400 | the body isn't a JSON object; the LLM is OpenAI's and you called `/anthropic`; or a System 1 question OpenAI's decision model can't take | Fix the request, or call `/openai/v1/chat/completions` |
| 413 | the body is over 32 MiB | Send less, such as smaller images |
| 503 | no model of that kind is chosen; its provider doesn't have it; or Outpost has no key for the provider | AI is unavailable for now; carry on without it, and try again later |
| 502 | the provider can't be reached | The same as 503 |
| 429 | the provider's rate limit, passed through with its `Retry-After` | Wait that long, then retry |
| anything else | the provider's own answer, passed through | As that API documents |

**Treat AI as an enhancement, not a dependency.** The operator may not have
chosen a model yet, may switch to one that's still downloading, may not give
it to you, or may stop Ollama, and a hosted provider can be down or out of
credit. Keep running, keep passing your healthcheck (§2), and do without:
show the recipe unsummarised, leave a message untriaged, queue the work for
later. Exiting, or reporting unhealthy, because AI is unavailable restarts
you (§5), which fixes nothing.

**Allow time.** With Ollama, the first request after a model is chosen, or
after Ollama has unloaded it, waits while Ollama loads it, which can take a
minute or more for a large model. Outpost waits up to 10 minutes for a
provider to start answering. Ollama also serves only a few requests to a
model at once and queues the rest, so a long generation for another app can
delay yours, and hosted providers have rate limits. Use generous timeouts,
and do AI work in the background, not in the request a user is waiting on,
wherever you can.

**Hosted models cost the operator money.** Outpost doesn't rate-limit you or
cache answers, so be economical: don't ask the same thing twice, don't send
more context than the answer needs, and prefer the System 1 model, which is
far cheaper, wherever a typed answer will do. Each request is one line in the
daemon's log: your service, the kind, the provider and model, the status, the
time taken and the token counts. Prompts and replies are never logged. The
operator sees your request counts under your app's Manage menu in the portal,
and decides there, or on the AI models page, which apps may use each kind.

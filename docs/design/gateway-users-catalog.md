# Design: users, a gateway, and an app catalog

Status: proposed, not built
Branch: `feat/users-and-catalog`
Date: 2026-10-25

## Summary

Outpost gains a small, built-in front door. One HTTP gateway inside the daemon
serves a **portal**, where people log in once and see a catalog of the apps
they can open. It also serves **each app on its own subdomain**, behind that
same login. The gateway tells each app who is calling, with plain headers for
simple apps and a short-lived signed token for apps that want to verify it.

Tailscale stops being a requirement, and new installs leave it off unless the
installer finds it on the machine and the operator says yes. The gateway works
on a LAN behind your own TLS, behind `tailscale serve`, or alongside today's
per-app tsnet nodes, which keep working unchanged.

Decisions already taken:

| Question | Decision |
|---|---|
| How apps are addressed | A subdomain per app: `yarr.outpost.example` |
| How apps learn the user | Identity headers plus an Ed25519-signed token, verifiable against a JWKS endpoint |
| How people log in | Username and password (argon2id), then a session cookie |
| Who can reach what | Every user reaches every app unless its manifest sets `allow`, which takes usernames and groups. Admins reach everything |
| Per-app `subdomain` override | Yes, from the start |
| Tailscale on new installs | Off by default; detected at install and upgrade, and offered then |
| Portal session length | 30 days, sliding |

## Goals

- Log in once and reach every Outpost app you are allowed to use, with no
  further prompts.
- Hand apps a user identity they can rely on, without making each app
  implement login.
- Land on a catalog that answers "what can I use here?" at a glance.
- Run with no Tailscale at all, or published through `tailscale serve`.
- Keep a service that knows nothing about this working exactly as before. The
  service contract only gains optional parts.

## Non-goals for this change

- **An OIDC provider.** Apps that only speak OIDC are a follow-up.
- **Passkeys, and logging in automatically from a Tailscale identity.** Both
  are follow-ups that build on the same session layer.
- **Personal access tokens for scripts and MCP clients.** Phase 4 below.
- **An admin web UI.** Users are managed with the CLI. The portal shows the
  catalog and an account page, nothing more.
- **Public internet exposure.** The gateway can be reached from a LAN or a
  tailnet. Funnel stays out of scope, as `spec.md` §1 has it.

## How it fits together

```
 browser ──HTTPS──▶ [ TLS: own cert, tailscale serve, or caddy ]
                              │
                              ▼
            ┌────────── outpost daemon ──────────────────────┐
            │  internal/gateway   (new)                       │
            │   ├─ Host: outpost.example   → portal           │
            │   │    /, /login, /logout, /account,            │
            │   │    /.well-known/jwks.json                   │
            │   └─ Host: <app>.outpost.example                │
            │        session check → allow check →            │
            │        strip + set identity headers →           │
            │        reverse proxy to 127.0.0.1:<port>        │
            │                                                 │
            │  internal/auth      (new)                       │
            │   users, groups, sessions, password hashes,     │
            │   signing keys        → ~/.outpost/auth.db      │
            │                                                 │
            │  internal/supervisor (existing)                 │
            │   service list + status, read via its public    │
            │   API, never via state.db                       │
            └─────────────────────────────────────────────────┘
```

Two new packages:

- **`internal/auth`** owns everything about people: users, groups, password
  hashes, sessions and token-signing keys. It knows nothing about HTTP.
- **`internal/gateway`** is the HTTP server: routing by host, the login
  handshake, the reverse proxy, identity injection and the portal pages. It
  reads the service list through the supervisor's public API, the same way the
  CLI does, so the "one core, multiple interfaces" rule holds. It never opens
  `state.db` or reads manifests.

### Why a separate `auth.db`

`state.db` is a cache of observed reality. Deleting it and running `apply`
again must converge, and there is a test that says so. Users and password
hashes are not observed reality; nothing can rebuild them. They go in their
own SQLite file, `~/.outpost/auth.db` (mode 0600), through the same
`modernc.org/sqlite` driver, so no cgo. The `state.db` invariant stays true,
and `auth.db` is documented as something to back up.

## Routing

The gateway is configured in `config.toml`:

```toml
[gateway]
enabled    = true
listen     = "127.0.0.1:8443"            # where the daemon listens
public_url = "https://outpost.example"   # what browsers see; sets the scheme
# Apps are served at <subdomain>.<domain>. Defaults to public_url's host.
domain     = "outpost.example"
# Serve TLS directly. Leave both empty when something in front terminates TLS.
tls_cert   = ""
tls_key    = ""
# Proxies allowed to set X-Forwarded-Proto and X-Forwarded-Host.
trusted_proxies = ["127.0.0.1/32", "::1/128"]
```

Each request is routed by its `Host`, with the port removed:

- **The portal host** (`domain`) serves the portal.
- **`<subdomain>.<domain>`** serves that app. `subdomain` defaults to the
  service name, so manifest validation now checks that the name, or an
  explicit `subdomain`, is a valid DNS label: lowercase, `[a-z0-9-]`, at most
  63 characters. Existing names that fail the check are an error only when the
  gateway is enabled, with a message naming the fix.
- **An app can claim a full hostname instead** with `hostname =
  "yarr.tail1234.ts.net"`. That's the escape hatch for Tailscale Services and
  for apps on unrelated domains (below).
- **Anything else** gets a bare 404 with no detail, so the gateway doesn't
  reveal which apps exist to someone probing hostnames.

The proxy is `httputil.ReverseProxy`, the same one the tsnet router already
uses. It handles WebSocket upgrades, and `FlushInterval: -1` keeps
server-sent events streaming. Healthchecks are unaffected: the supervisor
still probes `127.0.0.1:<port>` directly.

### DNS and certificates

Subdomain routing needs a wildcard: `*.outpost.example` resolving to the box,
and a wildcard certificate for HTTPS. That's the price of apps keeping their
root paths. The docs will cover three setups:

1. **LAN with your own domain.** A wildcard DNS record, and a wildcard
   certificate from your ACME client, or Caddy in front doing DNS-01.
2. **LAN without a domain.** `outpost.home.arpa` with entries in
   `/etc/hosts` or the router's DNS, and a locally trusted certificate (for
   example from mkcert).
3. **Tailscale serve.** See the Tailscale section, which has a real
   limitation.

## Login, sessions and single sign-on

### Why not one cookie on the parent domain

The obvious design sets the session cookie with `Domain=outpost.example`, and
every subdomain sees it. It works for case 1 above, but not in general.
`ts.net` is a shared domain, so on a tailnet the parent-domain cookie either
can't be set or reaches hosts it shouldn't. An app with its own `hostname`
has no shared parent at all.

### The handshake

Each host gets its own host-only cookie (`__Host-outpost`: `Secure`,
`HttpOnly`, `SameSite=Lax`, `Path=/`, no `Domain`). The portal hands each
cookie out through a one-time code. The user logs in once and the rest is
redirects:

1. A browser asks for `https://yarr.outpost.example/feeds` without a cookie.
   The gateway redirects to
   `https://outpost.example/login?app=yarr&next=/feeds`.
2. **Not logged in at the portal:** show the login form. On success, set the
   portal's own cookie, a new random token, which also prevents session
   fixation.
3. **Logged in at the portal**, now or already: check `allow` for `yarr`. If
   access is granted, mint a one-time code (32 random bytes, 60-second
   lifetime, single use, bound to user, app and session). Then redirect to
   `https://yarr.outpost.example/.outpost/callback?code=…&next=/feeds`.
4. The app host trades the code for an app-scoped session, sets its own
   cookie, and redirects to `next`.

A second app costs two redirects and no typing. The redirect target is never
taken from the request: `app` is looked up among known services, and `next`
must be a path (starting with `/` and not `//`), so there's no open redirect.

### Sessions

- Tokens are 32 random bytes. `auth.db` stores only their SHA-256, so a leaked
  database doesn't yield usable cookies.
- The portal session lasts 30 days, sliding: reissued when it's more than a
  day old. Each app session is tied to the portal session, so ending one ends
  the others.
- **Logout** (POST, from the portal or `/.outpost/logout` on any app host)
  ends the portal session and every app session derived from it.
  `outpost user logout <name>` ends every session that user has.
- Changing a password, disabling a user or removing a user ends all of that
  user's sessions.

### Passwords

- Hashed with argon2id from `golang.org/x/crypto/argon2`, which is pure Go and
  already in the module graph. Parameters are memory 64 MiB, time 3,
  threads 1, a 16-byte salt and a 32-byte key. The parameters are stored with
  each hash, so they can be raised later; a successful login rehashes with the
  current ones.
- Minimum length 12, no composition rules. Passwords are set through the CLI
  prompt (no echo), or chosen by the user on the account page.
- **Throttling:** failed logins back off per username and per client address
  (1s, 2s, 4s … capped at 60s). A wrong username and a wrong password take the
  same time and get the same message.

### First run

With the gateway enabled and no users yet, `outpost serve` logs a one-time
setup URL: `https://outpost.example/setup?token=…`. It's valid until the
first admin is created or the daemon restarts. `outpost user add --admin` does
the same job from the terminal.

### CSRF and framing

- Every POST on the portal and on `/.outpost/*` checks that `Origin`, or
  `Referer` when `Origin` is absent, matches the host. Together with
  `SameSite=Lax` this covers login, logout and the account page without hidden
  token fields.
- Portal pages send `Content-Security-Policy: default-src 'self';
  frame-ancestors 'none'` and `X-Content-Type-Options: nosniff`. They also send
  HSTS once `public_url` is https.
- The gateway sets no such headers on app responses; those belong to the app.

## What apps receive

### On every proxied request

The gateway **removes** any incoming `X-Outpost-*` header and the Outpost
session cookie, so a client can't forge identity and an app never sees
Outpost's credentials. It then sets:

| Header | Example | Notes |
|---|---|---|
| `X-Outpost-User` | `alice` | Username: stable, unique, safe to use as a key |
| `X-Outpost-Name` | `Alice Example` | Display name; may be empty |
| `X-Outpost-Email` | `alice@example.com` | May be empty |
| `X-Outpost-Groups` | `family,admins` | Comma-separated, sorted |
| `X-Outpost-Identity` | `eyJhbGciOiJFZERTQSIs…` | Signed token (below) |

It also sets the usual `X-Forwarded-For`, `X-Forwarded-Host` and
`X-Forwarded-Proto` (`SetXForwarded`, as now), so an app that builds absolute
URLs builds reachable ones.

### Why a signed token too

An app listens on loopback, so another process on the same machine could
connect to it directly and send any headers it likes. For most personal
setups that's an acceptable risk, and the plain headers are enough. An app
that wants assurance verifies `X-Outpost-Identity` instead.

- **Format:** a JWT signed with EdDSA (Ed25519). The claims are `iss` (the
  portal URL), `aud` (the app's hostname), `sub` (the username), `name`,
  `email`, `groups`, `iat`, and `exp` 60 seconds out. To keep per-request cost
  negligible, the gateway reuses a token per (user, app) for up to 30 seconds.
- **Keys** live in `auth.db`. The public half is served at
  `https://outpost.example/.well-known/jwks.json`, and on loopback at
  `http://127.0.0.1:<gateway port>/.well-known/jwks.json`, so an app can fetch
  it without leaving the machine. `outpost auth rotate-key` adds a new key and
  keeps the old one in the JWKS for a day.
- **Environment:** every service process also receives `OUTPOST_AUTH=gateway`,
  `OUTPOST_ISSUER` and `OUTPOST_JWKS_URL`, so an app can tell it's behind the
  gateway and knows where the keys are. Like `OUTPOST_STORAGE_DIR`, these are
  appended last so a manifest can't override them.

### In the service contract

`outpost-service-spec.md` gains a section: "Behind the gateway, identity
arrives in these headers; verify `X-Outpost-Identity` if you need to be sure;
never trust these headers when `OUTPOST_AUTH` is unset." Section 10 currently
says "tailnet membership is the boundary", and it becomes conditional: true
without the gateway, false with it. The conformance checklist gains one
optional item. Nothing becomes required, so every conforming service stays
conforming.

## Access control

New, all-optional manifest fields:

```toml
title       = "Yarr"                     # catalog name; defaults to the service name
description = "RSS reader"               # one line in the catalog
icon        = "icons/yarr.png"           # path under ~/.outpost, or empty for a lettermark
subdomain   = "rss"                      # defaults to the service name
hostname    = ""                         # a full hostname instead of <subdomain>.<domain>
allow       = ["alice", "group:family"]  # empty means every user
hidden      = false                      # leave it out of the catalog (still reachable)
public_paths = ["/webhook/*"]            # skip login here; identity headers are still stripped
```

- **`allow`** is checked at the app host on every request, and during the
  handshake before a code is minted. A logged-in user who isn't allowed gets a
  plain "you don't have access to Yarr" page, not a redirect loop. **Admins
  reach every app and see every card**, whatever `allow` says; `allow` limits
  ordinary users. A user or group named in `allow`
  that doesn't exist is a warning in `outpost apply`, not an error, so a
  manifest can be written before the user exists.
- **`public_paths`** exists because webhooks, RSS feeds and some APIs carry
  their own credentials and can't follow a browser login. The patterns are
  path prefixes with an optional trailing `*`, nothing more general. Requests
  on these paths still have `X-Outpost-*` removed, so no identity is forged
  there either.
- **`outpost.toml`** (the release descriptor) may set `title`, `description`
  and `public_paths`, which `outpost add` copies into the generated manifest.
  It can't set `allow`, `hostname` or `subdomain`: those are the operator's
  decisions, not the publisher's.

## The catalog

`https://outpost.example/` after login, rendered on the server with
`html/template` and embedded with `embed`. No JavaScript framework, no build
step, nothing fetched from a CDN. It must work offline on a LAN.

- **One card per app** the user is allowed to open (minus `hidden`; an admin
  sees every app, and `hidden` ones too, marked as hidden). Each card
  shows the icon or a lettermark, the title, the description and the address.
  The whole card is the link.
- **Status on every card:** running, starting, or unavailable. Unavailable
  covers stopped, failed and unhealthy, shown greyed with a one-line reason.
  Users don't need restart counts; admins get those from `outpost status`.
- **Order:** running first, then alphabetical. A search box appears only past
  about a dozen apps; it is the only JavaScript, a few lines, and the page
  works without it.
- **Empty states:** "No apps here yet" for an admin, with the CLI command to
  add one; "You don't have access to any apps yet" for a user.
- **Header:** the user's name, a link to the account page (change password,
  sign out everywhere), and sign out.
- Light and dark from `prefers-color-scheme`, readable at phone width.

The portal also hosts the login, setup, access-denied and account pages, all
in the same style.

## Running with and without Tailscale

| Setup | How the gateway is reached | Status |
|---|---|---|
| **No Tailscale** (`tailscale.enabled = false`) | `listen = "0.0.0.0:443"` with your certificate, or behind Caddy/nginx on the LAN | Fully supported |
| **`tailscale serve` on the host** | `tailscale serve` forwards HTTPS for the machine's name to the gateway's loopback port | Portal works; apps need a hostname each (below) |
| **Per-app tsnet nodes** (today's default) | Unchanged: each app is its own tailnet device, with no Outpost login | Unchanged; gateway optional alongside |

**The Tailscale limitation.** MagicDNS gives a machine one name,
`box.tail1234.ts.net`, with no wildcard beneath it, so
`yarr.box.tail1234.ts.net` doesn't exist. There are two ways to give apps
their own hostnames on a tailnet:

1. **Tailscale Services.** A service name per app (`svc:yarr` →
   `yarr.tail1234.ts.net`), served by this host and pointed at the gateway.
   Each manifest sets `hostname = "yarr.tail1234.ts.net"`. Thanks to the
   handshake above, single sign-on works even though the hosts share no parent
   domain. *I haven't verified the current `tailscale serve --service` syntax
   or plan availability; this needs checking against your tailnet before the
   docs promise it.*
2. **Keep per-app tsnet nodes for the tailnet** and use the gateway only on
   the LAN.

An option to route the per-app tsnet nodes *through* the gateway, so the
tailnet also requires an Outpost login, is a follow-up. It's a small change
once #35 lands, but it changes behaviour that people rely on today, so it
should be opt-in and decided separately.

## Tailscale off by default, offered when found

New installs start with Tailscale off. When the installer finds Tailscale on
the machine, it offers to turn it on.

### Detection

Outpost runs the `tailscale` CLI, on `PATH` or at the macOS app path
(`/Applications/Tailscale.app/Contents/MacOS/Tailscale`), with `status --json`.
It reads `BackendState` and the tailnet name. "Detected" means the CLI exists
and the backend is `Running`. Outpost runs the CLI rather than linking
Tailscale's client library, so the daemon still links no `tailscale.com` code
outside `internal/tsnet/node`. The check has a two-second limit, and any
failure reads as "not detected", never as an error.

Detection only decides what to *offer*. Per-app tsnet nodes don't need the
system Tailscale at all, so its absence never blocks turning Tailscale on.

### When the question is asked

- **`outpost install`**, both on first install and when re-run after an
  upgrade. The README documents no upgrade step today; this change makes
  "replace the binary, re-run `outpost install`" that step. If `config.toml`
  has no explicit `tailscale.enabled`, it reports what it found ("Tailscale is
  running here, tailnet `tail1234.ts.net`") and asks "Give each service its
  own Tailscale identity? [y/N]". The answer is written to `config.toml`, so
  it's asked once. With no TTY, the answer is no unless the new `--tailscale`
  flag is passed; the new `--no-tailscale` says no explicitly.
- **`outpost config --tailscale on|off`** changes it at any time.
- **First run without `install`.** A daemon or CLI command that creates
  `~/.outpost` for the first time writes `config.toml` with `enabled = false`.
  If Tailscale is detected, it logs one line naming the command that turns it
  on.

### Existing installs keep their behaviour

An install with no setting has been running with Tailscale **on**, because
that's the built-in default today. Turning it off silently on upgrade would
take every app off the tailnet. So the built-in default for a home that has
no setting stays **on**, and "off for new installs" comes from the explicit
value written at install or first run. Re-running `outpost install` on such a
home asks the question with "Y" as the default, since it's already on.

This also leaves two existing tests untouched:
`TestLoad_MissingFileUsesDefaults` and `TestLoad_PartialFileKeepsDefaults`
pin "no setting means on". The simpler alternative, flipping the built-in
default, would mean editing those two tests, which needs your approval, and
would turn Tailscale off on upgrade for anyone without a config file.

## CLI

Every command goes through the daemon socket, as the rest of the CLI does.
Session ends must reach the daemon's memory, and `auth.db` has one writer.

```
outpost user add <name> [--admin] [--email E] [--display-name N] [--group G…]
outpost user passwd <name>            # prompts twice, no echo
outpost user rm <name>
outpost user list                     # name, groups, admin, last login, sessions
outpost user groups <name> +family -kids
outpost user disable|enable <name>
outpost user logout <name>            # end every session
outpost auth rotate-key
outpost gateway status                # listen address, public URL, routed hosts
outpost config --tailscale on|off     # explicit switch; see the Tailscale section
```

Passwords cross the 0600 Unix socket, the same boundary the CLI already
trusts. They're never written to a log or the activity feed. The feed gains
`login`, `login-failed` (throttled) and `user` events, recorded through
`s.event(...)` as the repo rule requires.

## Security notes

- **The gateway is the only thing that should be reachable.** Apps still bind
  loopback; the service contract already requires it.
- **Identity headers are removed before anything else**, including on
  `public_paths` and on 404s.
- **Host validation:** only configured hosts are routed. `X-Forwarded-Host`
  and `X-Forwarded-Proto` are honoured only from `trusted_proxies`.
- **Plain HTTP:** when `public_url` is http and `listen` isn't loopback, the
  gateway refuses to start unless `allow_insecure = true` is set, because
  passwords would cross the network in clear text. On loopback, as behind
  `tailscale serve`, HTTP is fine, since TLS ends in front.
- **Timing-safe comparisons** for codes and tokens, and argon2id's own
  constant-time verify for passwords.
- **Rate limits** on login, and on `/.outpost/callback` per client address.

## Testing

- **`internal/auth`:** hashing, including rehash when parameters change;
  session creation, expiry, sliding refresh and ending sessions on password
  change and removal; one-time code expiry and single use; key rotation and
  JWKS contents. All against a temporary SQLite file.
- **`internal/gateway`**, with `httptest` and a real backend:
  - host routing, including unknown hosts and ports in `Host`;
  - the full redirect handshake;
  - `allow` by user and by group;
  - `next` with `//evil.example` and absolute URLs is rejected;
  - forged `X-Outpost-*` headers are stripped, including on `public_paths`;
  - the Outpost cookie never reaches the app;
  - an app verifies the token with the JWKS;
  - CSRF on login and logout;
  - WebSocket upgrade and SSE streaming pass through.
- **End to end:** the existing `fakeservice` fixture gains a `/whoami` handler
  that echoes its identity headers, so a test can run the daemon with the
  gateway, log in and read back who the app saw.
- **Catalog:** golden HTML for the empty, single-app, mixed-status and
  restricted cases, and a check that a user never sees a card for an app they
  can't open.
- **Existing tests stay untouched.** Every new manifest field is optional and
  the gateway is off by default, so today's behaviour is the zero value.

## Delivery

Four PRs, each usable on its own:

1. **`internal/auth` plus the user CLI.** The store, hashing, sessions, keys,
   the `outpost user …` commands over IPC, and first-run setup. No HTTP.
   Tailscale detection and the install-time question ship here too: they're
   small and independent of the gateway.
2. **The gateway:** config, host routing, the reverse proxy, login, logout,
   the handshake, identity headers, the token and JWKS. The portal is a bare
   page listing links.
3. **The catalog:** the new manifest fields and validation, the catalog,
   account and access-denied pages, and the `outpost.toml` fields.
4. **Docs:** the service contract section, a README guide for each setup in
   the table, and the `tailscale serve` recipe once it's verified.

Follow-ups, not in this design: personal access tokens for scripts and MCP;
passkeys; automatic login from a Tailscale identity; routing tsnet nodes
through the gateway; OIDC for apps that need it; an admin page.

## Impact on `spec.md`

`spec.md` belongs to the repository owner, so this change doesn't edit it.
Sections that no longer hold once this lands:

- **§1 "Out of scope, v1":** "A web UI — TUI and CLI only". The portal is a
  small web UI. Management stays CLI and TUI only.
- **§3 diagram:** gains the gateway and the auth store.
- **§7:** tsnet becomes one way of exposing apps, not the only one.
- **§12:** gains the `user`, `auth` and `gateway` command groups.

## Resolved questions

1. **`subdomain` override:** in from the start, defaulting to the service
   name.
2. **Tailscale default:** off for new installs; detected and offered at
   install and upgrade. See "Tailscale off by default, offered when found".
3. **Session length:** 30 days, sliding.
4. **Admins and `allow`:** admins reach and see every app. `allow` limits
   ordinary users only.

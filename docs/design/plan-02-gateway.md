# Plan: PR 2, the gateway

The build plan for the second delivery step in
[`gateway-users-catalog.md`](gateway-users-catalog.md): an HTTP gateway in
the daemon that logs people in once and proxies each app on its own
subdomain, telling the app who is calling.

Branch: `feat/gateway`, stacked on `feat/auth-store` (PR #39), since it
builds on `internal/auth`. It retargets to `main` once #39 merges.

## What ships

- A `[gateway]` section in `config.toml`, off by default.
- `internal/gateway`: host routing, the reverse proxy, login, logout, the
  one-time-code handshake, first-run setup, identity headers, the signed
  identity token and the JWKS endpoint. The portal is a bare page of links.
- `outpost serve` starts the gateway when it's enabled and hands each service
  `OUTPOST_AUTH`, `OUTPOST_ISSUER` and `OUTPOST_JWKS_URL`.
- `outpost gateway status`, over IPC.

Not in this PR, as the design has it: the manifest fields (`allow`,
`subdomain`, `hostname`, `public_paths`, `title`, …), the catalog, the account
page and the access-denied page, which are PR 3. Until then every enabled user
reaches every app, and an app's subdomain is its service name. Also deferred:
`login` and `login-failed` activity events, which need a supervisor API for
events that aren't about a service; and the `/whoami` handler on the
`fakeservice` fixture, since the gateway tests use an `httptest` backend
instead.

## Config

```toml
[gateway]
enabled         = false
listen          = "127.0.0.1:8443"
public_url      = "https://outpost.example"
domain          = ""          # defaults to public_url's host
tls_cert        = ""
tls_key         = ""
trusted_proxies = ["127.0.0.1/32", "::1/128"]
allow_insecure  = false
```

`Validate` checks these only when `enabled` is true: `public_url` is an
absolute http(s) URL with no path, `domain` is a hostname, `tls_cert` and
`tls_key` come as a pair, and every `trusted_proxies` entry parses as a CIDR.
It refuses an http `public_url` with a non-loopback `listen` unless
`allow_insecure` is set. An empty `[gateway]` changes nothing for existing
configs, and the existing config tests stay untouched.

## `internal/auth` additions

- `TouchFamily(token)`: slides the session and, for an app session, its
  parent too. Using any app counts as using your login, so someone who only
  ever opens one app isn't logged out after 30 days. `TouchSession` keeps its
  current behaviour, which PR 1's tests pin.
- `RevokeFamily(token)`: given any session, ends its portal session and every
  app session under it. That's what logging out from an app host means.

## `internal/gateway`

| File | What |
|---|---|
| `gateway.go` | `Options`, `New`, `Serve`, `Close`, the top-level handler: strip identity, pick the host, dispatch |
| `routes.go` | the route table: name → port and status, refreshed from the supervisor's public `List` |
| `hosts.go` | effective host and client address, honouring `X-Forwarded-*` only from `trusted_proxies` |
| `portal.go` | `/`, `/login`, `/logout`, `/setup`, `/.well-known/jwks.json` |
| `app.go` | `/.outpost/callback`, `/.outpost/logout`, and the proxied path |
| `codes.go` | one-time codes: in memory, 60 s, single use, bound to user, app and portal session |
| `throttle.go` | login backoff per username and per client address, 1 s doubling to 60 s; callbacks get five free failures per address first, so a stale link or two doesn't lock anyone out |
| `jwt.go` | EdDSA token minting, cached per (user, app) for 30 s |
| `csrf.go` | the `Origin`/`Referer` check on every POST |
| `templates/` | login, setup and portal pages, embedded with `embed` |

The one-time codes live in memory rather than in `auth.db`. They last a
minute, a daemon restart ending them is harmless, and keeping them off disk
means a copy of `auth.db` never holds one.

The route table goes through `supervisor.List`, the same public API the CLI
uses, so the gateway never reads `state.db` or the manifests. It refreshes
every two seconds instead of on each request, so proxied traffic never waits
on the reconcile loop.

**Cookies:** `__Host-outpost` with `Secure` when `public_url` is https. On a
plain-http `public_url` (only allowed on loopback or with `allow_insecure`)
it's `outpost` without `Secure`, since browsers drop a `Secure` cookie set
over http.

**On every request, before anything else:** remove incoming `X-Outpost-*`
headers, and the Outpost cookie from the `Cookie` header on anything
proxied.

## Tests

All new files, with `httptest` and a real backend that echoes what it
received:

- host routing, including ports in `Host`, unknown hosts (a bare 404), and
  `X-Forwarded-Host` honoured only from a trusted proxy;
- the full handshake with a cookie jar: app → portal login → code → app
  session → the app sees the right identity;
- a second app takes redirects but no login;
- `next` values such as `//evil.example`, `https://evil.example` and `/\evil`
  are refused;
- forged `X-Outpost-*` headers are stripped, and the Outpost cookie never
  reaches the app;
- the token verifies against the JWKS, and `aud` is the app's host;
- a code is single use, expires, and works only on its own app's host;
- CSRF: a POST from another origin, or with no `Origin` or `Referer`, is
  refused;
- throttling backs off and resets after a success;
- logout from an app host ends the portal session and its other app sessions;
- setup creates the first admin, and is closed once any user exists;
- WebSocket upgrade and SSE streaming pass through;
- config validation for each refusal above.

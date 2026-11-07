# Design: webhooks from the internet, through Tailscale Funnel

Status: built, on branch `feat/funnel-webhooks`. Designed 2026-11-03, built
2026-11-06. The operator's side is in the README under
[Funnel](../../README.md#funnel-webhooks-from-the-internet).

**What changed in building it.** The open questions were answered, and one
requirement was added:

- **Body cap:** a fixed 25 MiB, GitHub's ceiling, with no per-service field.
  A declared length over it is refused with 413 before the app sees any of
  it.
- **Public paths:** all or nothing per service, as proposed.
- **Catalog badge:** shown to admins only.
- **Set from the portal too, with the portal winning.** An admin's Internet
  access page sets an app's `public_paths` and `funnel`. The setting is kept
  in `exposure.db` and laid over the manifest when the supervisor loads it,
  so the router, the gateway and `outpost status` all see the same effective
  values. The manifest is the default for any app the page hasn't set, and
  "Use the manifest's setting" goes back to it. A setting is held to the
  manifest's own rules when it is saved and again on each apply. One that no
  longer passes, such as Funnel after the gateway is turned off, is logged and
  left out rather than stopping every other service from applying. Like
  `auth.db` and `secrets.db`, `exposure.db` is authoritative, an exception to
  the manifest being the source of truth, limited to these two fields.
- **One PR, not four.** The implementation plan below was done as one
  change, since the portal settings touch every layer.
- **The activity feed** records changes made in an app's Internet access setting in the portal.
  Funnel coming up or going down is logged by the daemon, not put in the
  feed, and refusals aren't aggregated there; each request is a line in the
  daemon log instead.

## Summary

Some apps need to hear from the public internet: GitHub, Stripe or a bot
platform calling a webhook. Today the only ways in are the tailnet and a LAN
behind the gateway. This design lets a service set `funnel = true` and have
**exactly its `public_paths`** answered on the public internet, at the same
address it has on the tailnet:

```toml
# ~/.outpost/services/tasking.toml
tailscale_name = "tasking"
public_paths   = ["/webhooks/github", "/hooks/*"]
funnel         = true
```

```
https://tasking.tailcad888.ts.net/webhooks/github   ← from anywhere
https://tasking.tailcad888.ts.net/                  ← tailnet only, behind the login, as today
```

Every other path, every other app, the portal, sign-in, `/mcp/<name>` and access
tokens stay unreachable from the internet. That holds because of where requests
go, not because of checks on each one: Funnel traffic arrives on a listener of
its own, goes to a gateway listener of its own, and that listener only has code
for public paths.

Decisions already taken:

| Question | Decision |
|---|---|
| What the internet can reach | The service's existing `public_paths`, nothing else. `funnel = true` exposes them, and the field stays the single list of login-free paths |
| Where webhooks arrive | The app's own tailnet host, `https://<tailscale_name>.<tailnet>.ts.net/<path>`, with the path unchanged |
| Delivery | This document first, then implementation PRs |

## What changes for `spec.md`

`spec.md` §1 lists Funnel as out of scope, and says (in §3) that
`funnel = true` is accepted by the schema but rejected until it's "built
deliberately later". This is that deliberate build, so it's an exception the
owner signs off by approving this doc. The spec itself stays the owner's to
edit. Until it's edited, README's *Deviations from the spec* records the
change. The MCP auth question that §3 ties Funnel to stays unanswered here:
`/mcp/<name>` is **not** exposed over Funnel (see Non-goals).

## Threat model

**What an attacker can do:** anyone on the internet can now open TLS
connections to an app's node on port 443 and send any HTTP request with any
headers, at any rate, including scanners that will find the hostname in
Certificate Transparency logs within minutes.

**What must stay true:**

1. **The internet reaches only declared public paths.** A request for anything
   else gets a 404, with no login redirect, no portal page and no hint about
   what's there.
2. **No identity crosses Funnel.** No session cookie, no `opat_` token and no
   `X-Outpost-*` header is honoured or passed through. A public request reads
   as anonymous to the app, as `public_paths` do today. A request carrying
   `Authorization: Bearer opat_…` is refused rather than passed on, because a
   leaked token must not become a way in from outside.
3. **Funnel can't be switched on by accident.** It needs `funnel = true` in the
   manifest, the gateway on, a tailnet name, at least one public path, and the
   tailnet policy's `funnel` attribute for that node. If any one is missing,
   it's off, and `outpost status` says why.
4. **A failure to start Funnel never takes the tailnet down.** The node keeps
   serving the tailnet. Funnel shows as unavailable, with the reason Tailscale
   gave.
5. **The app is still responsible for checking each webhook's signature.**
   Outpost can't verify a GitHub HMAC or a Stripe signature, because only the
   app has the secret. Outpost's job is to make sure only the paths the app
   chose are reachable, unmodified, with the body intact.

**Not in scope:** making an app's own webhook handler safe. A `public_paths`
entry over Funnel is as exposed as anything on the internet, and the README
will say so plainly.

## How a request travels

```
internet ──TLS──▶ Funnel relay ──▶ node: ListenFunnel(":443", FunnelOnly())
                                      │  (separate listener from the tailnet's ListenTLS(":443"))
                                      ▼
                                  gateway PUBLIC listener (127.0.0.1, its own port)
                                      │  host must be a funnel app's node name
                                      │  path must be one of its public_paths (same checks as isPublic)
                                      ▼
                                  the app on 127.0.0.1:<port>, anonymous

tailnet ──TLS──▶ node: ListenTLS(":443") ──▶ gateway (as today: login, tokens, MCP …)
```

### The node (`internal/tsnet/node`)

- **Two listeners on one port.** When started with `--funnel`, the node calls
  `server.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())` alongside its
  existing `ListenTLS("tcp", ":443")`. In tailscale.com v1.104.4, which
  `go.mod` pins, these are separate listener keys (`listenKey{…, funnel: true}`
  vs `false`), so they coexist, and a tailnet connection can never arrive on
  the Funnel listener, or the other way round. This is the line the whole
  design rests on, and a test pins it.
- **Where Funnel traffic goes.** Requests on the Funnel listener are proxied to
  a **second upstream**, `--public-upstream`: the gateway's public listener.
  They never go to the gateway's main listener. `onlyHost` still applies.
- **Client address.** Each Funnel connection is an `*ipn.FunnelConn` under the
  TLS layer, and its `Src` field is the real client address. The node sets
  `X-Forwarded-For` from that and nothing else, overwriting whatever the client
  sent. The gateway's public listener believes it only because only the node
  can reach that listener's loopback port. That's the same trust the main
  listener already gives `LocalUpstream`.
- **Starting Funnel.** `ListenFunnel` runs after the node reports ready, so a
  tailnet without the `funnel` attribute (or without HTTPS) still gets its
  tailnet service. The node reports the outcome on a status line, as it already
  does for the DNS name (`HostLinePrefix`), and the router records it in the
  status.
- **No Upgrade.** Requests on the Funnel listener with `Connection: upgrade`
  are refused with 400, because webhooks don't need WebSockets.

### The gateway (`internal/gateway`)

- **A second listener.** `gateway.PublicUpstream` is a new loopback listener
  served by its own handler, `servePublic`. That handler can only:
  1. strip identity, exactly as `ServeHTTP` does first today (`X-Outpost-*` by
     punctuation-insensitive match, plus the Outpost cookies);
  2. refuse any `opat_` bearer token or `Outpost-Token` header with 401, before
     looking anything up;
  3. find the route by the node's reported host (`HostLinePrefix`), and only
     among routes with `funnel = true`;
  4. require `isPublic(r, rt)`, the same function with the same `..`, `%2f`,
     `%25` and `;` hardening as today;
  5. hand the request to `proxyPublic`.

  Anything that fails a step gets a plain `404 Not Found` with an empty body,
  never a login redirect.
- **What `servePublic` never sees.** It has no portal routes, no
  `/mcp/<name>`, no `/.outpost/*`, no sign-in, no session lookup and no access
  tokens. None of that is wired into the handler, so a bug in a route can't
  expose any of it.
- **Limits:**
  - Request bodies are capped at **10 MiB** (`MaxBytesReader`) by default.
    GitHub can send up to 25 MB, so the cap needs to be adjustable; see Open
    questions.
  - Read-header timeout 10s, read timeout 60s, and a response timeout of 60s,
    since webhook senders give up well before that.
  - Per-client-IP rate limiting reuses the throttle the gateway already has
    (`throttle.go`): 404s and 401s count, and a client over the limit gets 429.
- **Activity feed.** Funnel requests are logged to the gateway log with the
  client address, method, path, status and size. The activity feed only gets a
  line when Funnel comes up or goes down for an app, plus refusals in
  aggregate (one line per window, the way `activityWindow` already batches
  failures), because scanners would otherwise flood it.

### The manifest (`internal/manifest`)

`funnel = true` stops being rejected, and is validated instead:

- **`tailscale_name` required:** there's no node to funnel through without one.
- **Gateway required:** without the gateway, a node proxies straight to the
  service, so Funnel would expose the **whole app**. This is checked where
  `Options.Gateway` is already known (`manifestOptions`). With the gateway off,
  it's an error, not a warning.
- **At least one public path required:** `funnel = true` with no
  `public_paths` is an error, since there's nothing to expose.
- **No new rules for the paths themselves.** `ValidPublicPath` already refuses
  `/*`, query strings, escapes and `/.outpost/`. A service whose public paths
  are too broad to put on the internet shouldn't set `funnel`.

The router's `Attach` stops rejecting `funnel` and passes `--funnel` and
`--public-upstream` to the node.

### What you set up in Tailscale

Funnel needs, for each node it's used on:

- HTTPS certificates enabled for the tailnet. They already are, since the
  nodes serve HTTPS today.
- The `funnel` node attribute in the tailnet policy. Tailscale's default
  policy grants it to `autogroup:member`, and since Outpost's nodes are owned
  by your user, that usually covers them. If the policy has been edited, add:

  ```json
  "nodeAttrs": [{ "target": ["autogroup:member"], "attr": ["funnel"] }]
  ```

  or narrow `target` to the specific user.

When the attribute is missing, `ListenFunnel` fails with Tailscale's own
message ("\"funnel\" node attribute not set"), and Outpost shows it as is.

## Visibility

- **`outpost status <name>` and the TUI** show `funnel: on`, `funnel: off
  (not set)` or `funnel: unavailable — <reason>`, followed by the public URLs,
  one for each public path.
- **The portal catalog** marks an app whose Funnel is on with a small "public"
  badge, so it's visible at a glance which apps the internet can reach.
- **`outpost apply`** prints the public URLs it's about to open the first time
  a service gains `funnel = true`.

## Testing

- **The node:** through the existing `startNode` seam, a fake server that
  records which listener a request arrived on. Funnel traffic must only reach
  the public upstream. A test pins `FunnelOnly` + `ListenTLS` on the same port
  against the pinned tsnet version, so a Tailscale upgrade that changes it
  fails loudly.
- **The gateway's public listener, refusing:** other apps' hosts, the portal
  host, non-public paths, `/mcp/x`, `/.outpost/*`, `..`, `%2f` and `%25`
  variants, `opat_` bearers, `Outpost-Token` and `X-Outpost-User` all get a 404
  or 401, never a redirect.
- **The gateway's public listener, passing through:** a matching path reaches
  the app with no identity headers and no Outpost cookie, the body intact and
  a correct `X-Forwarded-For`.
- **The manifest:** each rule above, plus each existing test still passing
  unchanged, apart from the two that assert `funnel = true` is rejected
  (`manifest_test.go` "funnel not supported" and
  `router_test.go` `TestAttach_RejectsFunnel`). **Those two would need your
  authorization to change**, because funnel stops being rejected.
- **End to end against `fakeservice`:** a public path answers on the public
  listener, and everything else 404s.

## Implementation plan

1. **Manifest:** validation of `funnel`, and README deviation notes. This
   needs the two test changes above.
2. **Gateway:** `PublicUpstream`, `servePublic` and limits, with tests.
3. **Node and router:** the Funnel listener, the second upstream, status
   reporting, and `--funnel`.
4. **Visibility:** status, TUI, the catalog badge and `apply` output.

Each is a separate PR that leaves `main` working. Funnel only switches on at
step 3.

## Non-goals

- **`/mcp/<name>` over Funnel.** claude.ai's web and mobile connectors would
  need it, but they also need OAuth (Dynamic Client Registration), which is
  `spec.md` §3's open question. Bearer tokens over the internet without that
  are a separate decision.
- **Exposing a whole app, or the portal, publicly.**
- **Ports other than 443.** Funnel also allows 8443 and 10000, but the app's
  own address is the point.
- **Verifying webhook signatures in Outpost.**

## Open questions

1. **`funnel_max_body`:** a per-service override of the 10 MiB cap, or a fixed
   25 MiB, matching GitHub's ceiling, for everyone?
2. **Should a public path be able to stay tailnet-only while another goes
   public?** Today, with `public_paths` reused, it's all or nothing per
   service. If that becomes a problem, a later `funnel_paths` subset can be
   added without breaking anything.
3. **The catalog badge:** should it be shown to every user, or only admins?

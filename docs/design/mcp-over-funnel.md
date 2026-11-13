# Design: an app's MCP endpoint from Claude's connectors, through Funnel

Status: proposed, 2026-11-13. Nothing here is built. It reverses one of the
non-goals in [funnel-webhooks.md](funnel-webhooks.md), so it needs the owner's
sign-off before any code.

## Summary

Claude's custom connectors (claude.ai, Claude Desktop's connectors, and the
iPhone and Android apps) connect to an MCP server **from Anthropic's cloud,
never from the user's device**. The server has to be reachable from the public
internet. Today an app's MCP endpoint is tailnet-only (`/mcp/<name>` on the
portal, or the app's `mcp_path` on its own host), and Funnel deliberately
carries neither. So Sous's recipes can be reached from Claude Code on a machine
on the tailnet, but not from Claude on a phone.

This design lets an app opt in to having **exactly its MCP endpoint** answered
on the internet, at its own Funnel address, for **access tokens made for the
purpose**:

```toml
# ~/.outpost/services/sous.toml
tailscale_name = "sous"
mcp_path       = "/mcp"
mcp_funnel     = true
```

```sh
outpost token create sroberts --name claude-phone --app sous --internet --expires 90d
```

```
https://sous.tailcad888.ts.net/mcp   ← from anywhere, with an internet token for sous
https://sous.tailcad888.ts.net/      ← tailnet only, behind the login, as today
```

The connector is added on claude.ai with that URL and a request header
`Authorization: Bearer opat_…`. The phone then uses it like any other
connector.

### Why the old non-goal can go

`funnel-webhooks.md` ruled out MCP over Funnel because "claude.ai's web and
mobile connectors would need it, but they also need OAuth (Dynamic Client
Registration)". That premise has changed. Connectors can now send fixed
credentials instead of OAuth: under **Request headers**, Claude "sends it on
every request to your server", "exactly as you enter it", and `Authorization`
is one of the names offered. So Outpost's existing bearer tokens are enough,
and Outpost doesn't need to become an OAuth server.

## Step 0: confirm the account can use it

Anthropic's documentation says request-header authentication "is in beta and
available to a limited set of organizations". **Before anything here is built,
check that adding a custom connector on claude.ai offers a Request headers
section.** If it doesn't, this design doesn't help yet, and the only way in is
OAuth. That's a much larger design (see Alternatives) and not this one.

## What changes for `spec.md`

`spec.md` §3 accepts `funnel = true` only once "the MCP auth question (see the
parent server-mode spec, Part 1 §4) is actually answered". This design is an
answer for one client, Claude's connectors: a per-app opt-in and a per-token
opt-in, with no OAuth. It's an exception the owner signs off by approving this
note. The spec stays the owner's to edit. Until it's edited, README's
*Deviations from the spec* records the change.

## Threat model

**What changes for an attacker:** today a leaked access token is useless
outside the tailnet. With this design, a token marked for the internet can
reach **one app's MCP endpoint** from anywhere, as its user, with every tool
that app offers, writes included. For Sous that means reading, creating,
editing and deleting recipes as you.

**What must stay true:**

1. **Only opted-in apps, and only their MCP path.** The internet reaches an
   app's `mcp_path`, exactly, on its own node's Funnel listener, and only when
   the app sets `mcp_funnel = true`. Every other path, app, the portal,
   sign-in and `/mcp/<name>` on the portal stay as they are: tailnet-only, or
   the app's `public_paths` if it has Funnel for webhooks.
2. **No existing token becomes an internet credential.** Only a token created
   with `--internet` works over Funnel. Every other token is refused there,
   exactly as today, so a token sitting in a shell history or a script doesn't
   gain reach.
3. **An internet token is narrow.** It has to name exactly one app and have an
   expiry. `outpost token create --internet` refuses one without `--app`, with
   more than one `--app`, or without `--expires`. An internet token also works
   on the tailnet, like any token for that app, so one connector token is all
   anyone needs.
4. **Nothing falls back.** Over Funnel the MCP path takes a token and nothing
   else: no session cookie, no public path, no login redirect. A missing, bad,
   expired, out-of-scope or non-internet token gets the same plain `401`,
   counted against the client's address. That's the same rule `/mcp/<name>`
   follows on the portal.
5. **Shutting someone out still works.** Disabling or removing a user, or
   `outpost user logout`, already ends their tokens through `endUserAccess` and
   the cascades. Internet tokens are ordinary tokens in `auth.db`, so this
   carries over unchanged.
6. **Identity is still the gateway's to set.** The request reaches the app with
   the same identity headers and identity token as on the tailnet, set inside
   the proxy's `Rewrite`, after `stripIdentity`. The token itself never reaches
   the app.

**Not in scope:** what an app's own MCP tools do. An app that opts in should
treat its MCP endpoint as internet-facing input, from an authenticated user.

**Source addresses.** Anthropic publishes the ranges its outbound MCP calls
come from (`160.79.104.0/21` and `153.61.208.0/20` as of 2026-11-13, "stable"
and "will not change without notice"). They're shared by every Anthropic
customer, so limiting to them stops scanners and token guessing from anywhere
else, but not a stranger with a Claude account and a stolen token. See Open
questions.

## How a request travels

```
Claude (Anthropic's cloud) ──TLS──▶ Funnel relay ──▶ sous node: ListenFunnel(":443", FunnelOnly())
                                                        │
                                                        ▼
                                          gateway PUBLIC listener (servePublic)
                                                        │  host = a node with mcp_funnel on
                                                        │  path = exactly its mcp_path
                                                        │  exactly one token, marked internet,
                                                        │  scoped to this app alone
                                                        ▼
                                          sous on 127.0.0.1:<port>, as the token's user
```

The Funnel side is what `funnel-webhooks.md` built: its own listener on the
node, and its own gateway listener whose handler has only what is wired into
it. This design adds one branch to that handler and nothing to the tailnet
path.

### The gateway (`internal/gateway/public.go`)

`servePublic` keeps its order: strip identity, throttle, refuse tokens, match
the host, require a public path. One step goes in **before the token
refusal**:

- If the host's route has `mcp_funnel` on and the path is exactly its
  `mcp_path`, matched the way `mcpService` matches (escaped form equal to the
  path, so no `%2f`, `..` or `;` tricks), the request goes to a new
  `serveFunnelMCP`. Nothing else does.

`serveFunnelMCP` does what `serveMCP` does on the portal, with three
differences:

1. **The client address is the Funnel client's** (`publicClient`), so the token
   throttle counts the real sender and not the node.
2. **The token must be marked internet and scoped to this app alone.**
   Otherwise it's refused with the same `401` as a bad token.
3. **The response goes out on the app's own origin**, so `onPortalOrigin`'s
   rewriting isn't needed. `Set-Cookie` is still dropped, as on the portal's
   MCP route.

What `servePublic` already enforces still applies: no `Upgrade` (Streamable
HTTP doesn't use one), the 25 MiB body cap, and the Funnel timeouts. A
`DELETE` to end an MCP session goes the same way as a `POST`.

**Timeouts.** Funnel's write timeout is 75 seconds. A tool call answers well
inside it. The optional long-lived `GET` stream Streamable HTTP allows for
server-to-client messages would be cut off at 75 seconds and has to
reconnect, which the transport expects. That's acceptable unless an app is
found to rely on a long stream.

### Tokens (`internal/auth`, `cmd/outpost/token_cmd.go`)

- **`auth.db`:** access tokens gain an `internet` flag, a new column defaulting
  to false, so every existing token stays tailnet-only. `auth.db` is
  authoritative, so this is a migration, not a cache rebuild.
- **`outpost token create --internet`:** requires exactly one `--app` and an
  `--expires`. Open question: a maximum lifetime?
- **`outpost token list`:** shows which tokens work from the internet. Tokens
  are managed only from the CLI today, and this keeps it that way.

### The manifest and the node

- **`mcp_funnel = true`** is validated like `funnel`: it needs the gateway on,
  a `tailscale_name` and an `mcp_path`. Unlike `funnel`, it needs no
  `public_paths`.
- **The node's Funnel listener** runs when either `funnel` or `mcp_funnel` is
  on. The node needs no other change, since the gateway's public listener
  decides what is answered.
- **The portal's Internet access page** gains the same switch, held in
  `exposure.db` over the manifest the way `funnel` already is.

### Visibility

- **`outpost status <name>`:** `mcp over funnel: on https://sous.<tailnet>.ts.net/mcp`,
  or why it's unavailable.
- **The catalog's public badge** also covers an app whose MCP endpoint is on
  the internet.
- **The activity feed:** a line when an internet token is first used from the
  internet. Refusals stay out of it, as Funnel's already do, since scanners
  would flood it. Each request is a line in the daemon log, with the client
  address.
- **`outpost apply`** prints the MCP URL the first time an app gains
  `mcp_funnel`.

## Testing

All new tests. **No existing test needs to change.** `TestPublic_RefusesAccessTokens`
sends a token to a webhook path on an app without `mcp_funnel`, and that stays
a `401`.

- **Through `servePublic`:** a good internet token for the app reaches it at
  its `mcp_path` with identity set and no token. A tailnet-only token, an
  unscoped token, a token for another app, a token whose user is disabled, an
  expired token, two tokens, and no token all get the same `401`. Every other
  path on that host, with or without a token, stays a `404`, and so does the
  MCP path on an app without `mcp_funnel`. `%2f`, `..`, `;` and trailing-slash
  variants of the path are `404`. `Set-Cookie` from the app never reaches the
  client. The throttle counts the `X-Forwarded-For` client.
- **Tokens:** `--internet` without `--app`, with two, or without `--expires` is
  refused. The migration leaves existing tokens tailnet-only.
- **Manifest:** `mcp_funnel` without the gateway, a tailnet name or an
  `mcp_path` is an error.
- **End to end:** `fakeservice` behind the public listener, as the Funnel tests
  already do.

## Implementation plan

1. **Tokens:** the `internet` flag, its migration, and the `create` and `list`
   changes. Nothing can use it yet.
2. **Gateway:** `serveFunnelMCP` and its branch in `servePublic`, with tests.
3. **Manifest, router and node:** `mcp_funnel`, and the node's Funnel listener
   running for it.
4. **Visibility and the portal:** status, the badge and the Internet access
   switch.
5. **Docs:** the README section, the deviation note, and replacing the
   README's statement that connectors in Anthropic's cloud can't use Outpost.

The internet can reach nothing new until step 3.

## Alternatives considered

- **`/mcp/<name>` on the portal, through a Funnel on the portal's node.** It
  would put the portal's host on the internet and make one address the way in
  to every app's MCP. The app's own address keeps the exposure per app, and is
  what Funnel already does.
- **OAuth with Dynamic Client Registration.** It would work without the beta,
  but it means Outpost becoming an authorization server, with consent pages,
  client registration, refresh tokens and their revocation. It's worth its own
  design if request headers stay limited.
- **A separate tunnel, such as Cloudflare, straight to the app.** It bypasses
  the gateway, and with it identity, tokens, the allow list and the throttle.
- **Tailscale on the phone.** It doesn't help: the phone never opens the
  connection.

## Open questions

1. **Source addresses:** accept only Anthropic's published outbound ranges on
   the MCP path, as a per-app `mcp_funnel_from` with those ranges as the
   default, or leave it to the token? Limiting cuts scanner noise to nothing,
   but a change to the ranges would break the connector until the setting is
   updated.
2. **Token lifetime:** cap internet tokens, at 90 days for instance, or only
   require that they have an expiry?
3. **Field name:** `mcp_funnel`, or a `funnel_mcp` beside `funnel` so the two
   sort together?
4. **Read-only:** should Outpost offer to pass only a subset of an app's tools?
   It can't tell an MCP read from a write without parsing JSON-RPC, so as
   proposed this is the app's job. An app could offer a read-only `mcp_path` of
   its own.

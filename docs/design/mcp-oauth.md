# Design: signing Claude's connectors in with OAuth

Status: proposed, 2026-11-15. It builds on
[mcp-over-funnel.md](mcp-over-funnel.md), which is built, and replaces that
note's "OAuth with Dynamic Client Registration" alternative with a smaller
one.

## Summary

[mcp-over-funnel.md](mcp-over-funnel.md) puts an app's MCP endpoint on the
internet for access tokens made with `--internet`. Connecting Claude to it
takes a token made on the command line and pasted into the connector's
**Request headers**. In practice that is awkward. Claude reserves
`Authorization` for its own OAuth token, so the header has to be
`Outpost-Token`. Claude also sees the `401` and marks the server as OAuth
("Detected"), so the user has to know to pick **No sign-in**.

This design makes Outpost the OAuth authorization server for an app's MCP
endpoint, so connecting is:

1. Add a custom connector on claude.ai with `https://sous.<tailnet>.ts.net/mcp`
   and nothing else.
2. Claude opens Outpost's sign-in in your browser. You sign in as usual, if
   you aren't already, and press **Allow**.
3. Claude is connected, and stays connected. It renews its own tokens.

**The sign-in stays on the tailnet.** In OAuth's authorization code flow, the
page you sign in on is opened by *your browser*, not by Anthropic's servers.
Only discovery and the token endpoint have to be reachable by Anthropic. So:

| Address | Who calls it | Where it is |
|---|---|---|
| `/.well-known/oauth-protected-resource…` | Claude's servers | the app's Funnel host, internet |
| `/.well-known/oauth-authorization-server` | Claude's servers | the app's Funnel host, internet |
| `/.outpost/oauth/token` | Claude's servers | the app's Funnel host, internet |
| `/oauth/authorize` | your browser | the portal, tailnet only |
| the MCP endpoint itself | Claude's servers | the app's Funnel host, internet (built) |

The login page, passwords, sessions and the consent page never go on the
internet. The cost is that you connect, or reconnect after revoking, from a
device on your tailnet: a laptop, or a phone with Tailscale on. After that,
Claude renews tokens server to server and works from anywhere.

**Clients identify themselves with a Client ID Metadata Document (CIMD).**
Claude's connector settings offer "Use Claude's published identity", described
as "The server reads Claude's client details from a URL Anthropic hosts
(CIMD)". It is marked Recommended. Outpost fetches that document, checks it,
and keeps nothing per client. There is no Dynamic Client Registration: no
registration endpoint, no stored client records, and no secrets.

## What the specs require

Facts below are from the MCP authorization spec, version 2025-11-25
(<https://modelcontextprotocol.io/specification/2025-11-25/basic/authorization>),
and the CIMD Internet-Draft, -03 of 2026-11-02
(<https://datatracker.ietf.org/doc/draft-ietf-oauth-client-id-metadata-document/>).

- **Protected resource metadata (RFC 9728)** is required. The server announces
  it with `resource_metadata` in the `401`'s `WWW-Authenticate`, or at the
  well-known URI, path-based (`/.well-known/oauth-protected-resource/mcp`) or
  at the root. Outpost does both.
- **Authorization server metadata (RFC 8414)** at
  `/.well-known/oauth-authorization-server` for an issuer with no path. It
  must list `code_challenge_methods_supported` including `S256`; without it, MCP
  clients "MUST refuse to proceed". `client_id_metadata_document_supported:
  true` tells the client to use CIMD.
- **PKCE with S256** is required.
- **The `resource` parameter (RFC 8707)** is sent with both the authorization
  and token requests, as the canonical URI (`https://sous.<tailnet>.ts.net/mcp`).
  The server must check that a token was issued for it.
- **Refresh tokens for public clients must be rotated.**
- **Redirect URIs** must be matched exactly against the client's registered
  ones (CIMD §4.2), and the authorization response must include `iss`
  (RFC 9207, CIMD §7).
- **Fetching a client's document** must not follow redirects (CIMD §6), must
  not cache errors or invalid documents, and the draft's security section
  says not to fetch special-use addresses (SSRF) and to cap the size, at 5 KB
  recommended. The `client_id` in the document must equal the URL it was
  fetched from, compared as strings.
- **No token passthrough.** The app never sees the client's token. The gateway
  already removes it.

**What Anthropic doesn't document.** Claude's redirect URI, the URL of its
client metadata document, its token lifetimes and how it handles a refresh
failure aren't in any documentation found. The design depends on none of them
in advance. The document says what the redirect URI is, and the
document's address arrives as the `client_id`. Both are logged the first time,
so step 0 below confirms them.

## Threat model

**What changes for an attacker.** Compared to internet tokens:

- **New code on the internet:** the discovery documents, which are static, and
  the token endpoint, which parses a form and checks a code or refresh token.
  The authorization endpoint is not on the internet.
- **A refresh token is a new long-lived credential**, held by Anthropic. It's
  worth what an internet token is: one app's MCP endpoint, as one user. It is
  rotated on every use, and reusing an old one revokes the whole grant, so a
  stolen copy works at most until the real client next refreshes.
- **A consent page is a new phishing target.** Someone could add a connector
  of their own pointing at your app, and send you its authorize link. They
  would need you to open it on your tailnet, while signed in, and press Allow.
  The consent page names the client by its document's host, not just its
  self-chosen name, and says exactly what is granted.

**What must stay true:**

1. **Only opted-in apps.** Discovery and the token endpoint exist only on the
   Funnel host of an app with `mcp_funnel` on, next to its MCP endpoint, and
   nothing else of the app is added. An app's host without `mcp_funnel` is
   what it was: a `404`.
2. **Only you grant access, and only from the tailnet.** `/oauth/authorize`
   is on the portal, which isn't on the internet. It needs a signed-in session,
   the app's `allow` list, a fresh form submission from the portal's own
   origin, and the Allow button.
3. **A grant is as narrow as an internet token.** One user, one app, one
   client. The access tokens it issues are internet access tokens scoped to
   that app, built in [mcp-over-funnel.md](mcp-over-funnel.md) steps 1–2, so
   every check there applies unchanged, the token before the throttle
   included.
4. **Short access tokens, rotating refresh tokens.** Access tokens last an
   hour. A refresh token is single-use. Presenting one that was already used
   revokes the grant and its access tokens, and says so in the activity feed.
5. **Shutting someone out still works.** `endUserAccess` deletes the user's
   grants along with their sessions and tokens. That covers disabling and
   removing a user, `outpost user logout`, signing out everywhere and changing a
   password. A grant can also be revoked on its own from the CLI or the
   account page.
6. **Fetching a client's document can't be turned against the machine.** Only
   `https`, only public addresses (checked on the address actually dialled),
   no redirects, a 5-second timeout, a 5 KB cap. The fetch happens on the
   tailnet-only authorize request, and the token endpoint never fetches.
7. **Codes are single-use, short and bound.** An authorization code lasts 60
   seconds, is used once, and is bound to the user, app, client, redirect URI,
   `resource` and PKCE challenge. It is kept in memory only, so a restart
   drops codes in flight, which only means pressing Allow again.

**Not in scope:** an internet-facing sign-in; Dynamic Client Registration;
pre-registered or confidential clients; OAuth for the portal or `/mcp/<name>`;
scopes finer than "the app's MCP endpoint" (see Open questions).

## How a connection is made

```
claude.ai (Anthropic)                     you (browser on the tailnet)              Outpost
──────────────────────                    ────────────────────────────              ───────
POST https://sous…/mcp  ───────────────────────────────────────────────────────▶ 401
   WWW-Authenticate: Bearer resource_metadata="https://sous…/.well-known/oauth-protected-resource/mcp"
GET  …/oauth-protected-resource/mcp ───────────────────────────────────────────▶ {resource, authorization_servers:[https://sous…]}
GET  https://sous…/.well-known/oauth-authorization-server ─────────────────────▶ {issuer, authorization_endpoint: https://outpost…/oauth/authorize,
                                                                                    token_endpoint: https://sous…/.outpost/oauth/token, S256, CIMD}
open authorize URL in your browser ──▶ GET https://outpost…/oauth/authorize?client_id=…&redirect_uri=…&code_challenge=…&resource=…&state=…
                                          sign in if needed; Outpost fetches client_id, checks redirect_uri
                                          consent page: "Claude (claude.ai) wants to use Sous as sroberts"
                                       ──▶ POST Allow  ─────────────────────────────▶ code (60s, in memory)
                                       ◀── 303 to redirect_uri?code=…&state=…&iss=https://sous…
POST https://sous…/.outpost/oauth/token  grant_type=authorization_code ────────▶ {access_token: opat_… (1h), refresh_token, expires_in}
POST https://sous…/mcp  Authorization: Bearer opat_… ──────────────────────────▶ serveFunnelMCP, as built
… an hour later …
POST …/oauth/token  grant_type=refresh_token ──────────────────────────────────▶ new access token, new refresh token
```

**The issuer is the app's Funnel host**, `https://sous.<tailnet>.ts.net`, one
per app. RFC 8414 allows the authorization endpoint to be on another host, so
it points at the portal. A per-app issuer keeps everything per app: a token
from Sous's issuer can't be presented to another app, `resource` has one
right answer per issuer, and the portal's host never appears in anything an
internet client fetches, other than the authorize address it hands to your
browser.

## The pieces

### auth.db: grants (step 1)

A new table, `oauth_grants`:

| Column | |
|---|---|
| `id` | never reused, like access tokens |
| `user_id` | `ON DELETE CASCADE` |
| `app` | the one app |
| `client_id` | the CIMD URL |
| `client_name` | from the document, for the list and the feed |
| `refresh_hash` | SHA-256 of the current refresh token |
| `previous_hash` | the one before, to recognise reuse |
| `created_at`, `refreshed_at`, `expires_at` | the grant lapses after 90 days without a refresh |

`access_tokens` gains `grant_id` (default `0`), so a grant's access tokens go
when it does, and `outpost token list` can leave them out. They're
renewed hourly and would fill the list. `endUserAccess` deletes the user's
grants. New store methods:

- `CreateGrant(user, app, clientID, clientName)` returns the grant, an access
  token and a refresh token.
- `RefreshGrant(refreshToken, clientID, app)` rotates. A `previous_hash` match
  revokes the grant (`ErrRefreshReused`).
- `ListGrants`, `RevokeGrant(id)`, and pruning lapsed grants and expired grant
  tokens.

`outpost token list` gets a second table, **Connected clients**, and `outpost
token revoke-client <id>` ends one. Both go over the socket like every token
command.

### The gateway's public listener (step 2)

`servePublic` gains three exact paths, each only on the Funnel host of an app
with `mcp_funnel` on, matched as `funnelMCPRoute` matches:

- `GET /.well-known/oauth-protected-resource` and
  `/.well-known/oauth-protected-resource<mcp_path>`: `resource`,
  `authorization_servers`, `bearer_methods_supported: ["header"]`,
  `scopes_supported: ["mcp"]`.
- `GET /.well-known/oauth-authorization-server`: `issuer`,
  `authorization_endpoint` (the portal's), `token_endpoint`,
  `response_types_supported: ["code"]`,
  `grant_types_supported: ["authorization_code", "refresh_token"]`,
  `code_challenge_methods_supported: ["S256"]`,
  `token_endpoint_auth_methods_supported: ["none"]`,
  `client_id_metadata_document_supported: true`,
  `authorization_response_iss_parameter_supported: true`,
  `scopes_supported: ["mcp"]`.
- `POST /.outpost/oauth/token`: `authorization_code` and `refresh_token`
  grants, errors as RFC 6749 §5.2 JSON, `Cache-Control: no-store`. Its
  refusals count in `publicThrottle`, like the listener's other refusals. A
  successful exchange isn't throttled, for the same reason a good token isn't:
  Anthropic's addresses are shared.

`/.well-known/openid-configuration` stays a `404`. Outpost isn't an OpenID
provider. Nothing changes on the tailnet side, and the portal keeps answering
discovery with a `404`, since `/mcp/<name>` doesn't do OAuth.

**Built differently:** the `401` from `serveFunnelMCP` stays a plain `Bearer`,
without `resource_metadata` or `scope`. The MCP spec lets a server announce
its metadata either in that header or at the well-known address, and clients
must try both. Claude was seen trying
`/.well-known/oauth-protected-resource/mcp` first, then the root and
`/.well-known/oauth-authorization-server`, before any of this was built.
Keeping the header as it is leaves the tests for mcp-over-funnel unchanged.

### The portal: authorize and consent (step 3)

`GET /oauth/authorize` on the portal:

1. **Checks that can't redirect**, because the redirect itself isn't trusted
   yet. Any failure is an error page, never a redirect:
   - `resource` names an app with `mcp_funnel` on, by its canonical URI.
   - `client_id` is an `https` URL with a path, no fragment, no dot segments,
     and not the issuer.
   - The document is fetched safely (invariant 6), and its `client_id`
     matches.
   - `redirect_uri` is one of the document's `redirect_uris`, exactly, and
     `https`.
2. **Checks that redirect with an error** to the now-trusted `redirect_uri`:
   `response_type=code`, a `code_challenge` with `S256`, and `scope`, if
   given, `mcp`.
3. **Sign-in.** If there's no session, the usual login, then back here.
   Someone the app's `allow` list refuses gets the "No access" page.
4. **Consent.** It names the client by `client_name` *and* the document's
   host, the app, and you. It says plainly that Claude will be able to use
   every tool the app has, as you, including changes, until you disconnect.
   **Allow** is a POST, checked with `sameOrigin` like every other portal
   form. **Deny** redirects with `error=access_denied`.
5. **Allow** makes the code and redirects to `redirect_uri` with `code`,
   `state` and `iss`.

The client document is cached for an hour when it was valid, and never when it
wasn't, as CIMD §6.2 asks.

### Visibility (steps 3 and 4)

- **The activity feed** gets one line per new grant ("sroberts connected
  Claude (claude.ai) to sous"), per revocation, and per refresh-token reuse.
  Refreshes and token-endpoint refusals stay in the daemon's log, as Funnel's
  do.
- **The account page** gets a **Connected clients** list: client, app, when
  connected, last renewed, and a **Disconnect** button.
- **`outpost status <name>`'s** `mcp funnel:` line says "OAuth or internet
  access tokens".

## Testing

All new tests, no existing test changed:

- **Store:** creating, rotating, reuse revoking, lapsing, cascades from user
  removal, disabling and logout, and a grant's tokens going with it.
- **Token endpoint, through `servePublic`:** the code exchange, with the right
  and wrong verifier, client, redirect URI and resource; a code used twice; an
  expired code; refresh and rotation; reuse; an app without `mcp_funnel`
  (`404`); throttling of refusals; and that a good exchange passes during
  backoff.
- **Discovery:** the exact documents, the `401` header, and `404` for an app
  without `mcp_funnel` and for `openid-configuration`.
- **Authorize:** each pre-redirect refusal shows a page and doesn't redirect;
  the redirect errors; login first; the `allow` list; consent, and a POST from
  another origin; Deny; `iss` and `state` coming back.
- **Client documents:** a fake HTTPS server for redirects, size, timeout,
  mismatched `client_id`, private addresses refused at dial time, and
  caching only of valid documents.
- **End to end:** the whole dance against the harness, ending in an MCP call
  that reaches the app as the user.

## Implementation plan

0. **Confirm Claude's side.** Once steps 1–3 are in, connect Claude to Sous and
   read in the daemon's log the `client_id`, redirect URI and refresh
   behaviour it actually uses. If Claude doesn't use CIMD after all, this
   design needs DCR, which it doesn't cover.
1. **auth.db: grants,** with the CLI's list and revoke. Nothing can create one
   yet.
2. **The public listener: discovery and the token endpoint,** tested with
   codes made directly.
3. **The portal: authorize, client documents and consent,** and the activity
   lines. After this, connecting works.
4. **The account page's Connected clients, the status line, and the README**,
   which replaces "Request headers with `Outpost-Token`" as the primary way to
   connect, keeping it as the fallback.

The internet reaches nothing new until step 2, and nothing can be granted
until step 3.

## Alternatives considered

- **Dynamic Client Registration.** It's what the earlier note assumed. It
  means a public registration endpoint and a stored client per registration,
  and Claude's own settings warn it "creates many client registrations on
  busy" servers. CIMD needs neither. If step 0 shows Claude needs DCR, it's a
  follow-up.
- **The authorization server on the portal's host.** One issuer for every app.
  But the token endpoint would then have to be on the internet at the
  portal's host, which [mcp-over-funnel.md](mcp-over-funnel.md) deliberately
  keeps off the internet.
- **An internet-facing sign-in on the app's Funnel host.** It would let you
  connect from a device off the tailnet. It puts a password form on the
  internet, which is a separate decision.
- **Keeping the fixed token only.** It works, and stays as the fallback, but
  it asks the user to know two workarounds.

## Open questions

1. **Which clients:** any CIMD client, shown on the consent page by its host
   (as proposed), or only an allow list of client hosts, such as `claude.ai`,
   once step 0 shows what Claude uses?
2. **Lifetimes:** one-hour access tokens and a 90-day idle limit on grants, as
   proposed, or an absolute limit as well?
3. **Opt-in:** OAuth on for every app with `mcp_funnel` (as proposed, since
   granting still needs you on the tailnet), or its own manifest field?
4. **Read-only scope:** as in the earlier note, Outpost can't tell an MCP read
   from a write, so a narrower scope would be the app's to offer.

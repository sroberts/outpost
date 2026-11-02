# Review flows

The portal's primary paths, for `/web-app-review`. Run against a throwaway home:
gateway on, Tailscale off, a few `fakeservice` apps. Never against the operator's
real home, and never with `outpost install`. Check each at 1280px and 390px.

## 1. First-run setup
1. With no users, open `/`. Expected: the sign-in page; setup isn't open to anyone who finds it.
2. Open the one-time setup link the daemon logs at start. Expected: the setup form.
3. Submit a mismatched or short password. Expected: an inline error, nothing created.
4. Submit a valid admin. Expected: signed in, on the catalog.

## 2. Sign in and out
1. Signed out, open `/`. Expected: the sign-in page.
2. Submit a wrong password. Expected: an error, no session.
3. Submit the right password. Expected: the catalog.
4. Sign out. Expected: back at sign-in; `/` no longer shows the catalog.

## 3. Catalog
1. Open `/`. Expected: one row per app with its state and a usage line in KB, MB or GB.
2. With 13 or more apps, type in the filter. Expected: rows narrow to matches; clearing restores them. It isn't there with fewer.
3. Open a row's usage panel. Expected: CPU, memory, traffic and storage sparklines.

## 4. Open an app through the gateway
1. Follow an app's link. Expected: the app answers at its own host with no second sign-in.
2. Go back and reopen the catalog within a minute or so. Expected: the app's traffic has grown; the sampler reads it every half minute.

## 5. App actions (admin)
1. Open a row's Manage menu and stop the app. Expected: a confirmation, then the row shows stopped.
2. Start it again. Expected: the row reads running without a manual reload.
3. Back it up. Expected: a result the row reports.
4. Open a usage panel during an action. Expected: the refresh still runs.

## 6. Updates page (admin)
1. Open `/updates`. Expected: an Outpost row, a row per app installed from a release, and a line naming the rest.
2. On a dev build. Expected: a one-line note and no update button for Outpost.
3. Fetch `/updates/self`. Expected: 200 JSON.
4. As a non-admin. Expected: `/updates` and `/updates/self` refuse.

## 7. Account
1. Open `/account`. Expected: username, sign-ins and the password form.
2. Change the password with a wrong current one. Expected: an error, password unchanged.

## 8. Not found and wrong method
1. Open an unknown path. Expected: the portal's "Nothing here" page with a link back.
2. GET `/apps`. Expected: 405 with "Use the buttons".

## 9. MCP through the portal (access tokens)
Needs a service with `mcp_path` that answers MCP: a small stub that echoes what it received, and sets a cookie, is enough. Drive it with curl; there's no page.
1. `outpost token create <user> --app <svc>`, then POST an `initialize` to `/mcp/<svc>` with `Authorization: Bearer`. Expected: 200 from the app, which saw its `mcp_path`, its own host (port included), the token's user, `aud` its own host, and no token, Outpost cookie, `Forwarded` or `X_Outpost_*` header.
2. Check the response headers. Expected: no `Set-Cookie` or `Location`; `Cache-Control: no-store`, `nosniff` and a sandbox CSP; `Mcp-Session-Id` passed through.
3. No token, a token scoped to another app, an unknown app and an app without `mcp_path`. Expected: the first a 401 with `WWW-Authenticate: Bearer` and no redirect; the other three the identical plain 401.
4. A user the app's `allow` refuses. Expected: 403. `/mcp/<svc>/x`: 404. A token on `/`: 401. `/.well-known/oauth-protected-resource`: plain 404.
5. GET `/mcp/<svc>` with `Accept: text/event-stream`. Expected: events arrive as the app sends them, not all at the end.

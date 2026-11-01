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
2. Type in the filter. Expected: rows narrow to matches; clearing restores them.
3. Open a row's usage panel. Expected: CPU, memory, traffic and storage sparklines.

## 4. Open an app through the gateway
1. Follow an app's link. Expected: the app answers at its own host with no second sign-in.
2. Go back and reopen the catalog. Expected: the app's traffic has grown.

## 5. App actions (admin)
1. Open a row's Manage menu and stop the app. Expected: a confirmation, then the row shows stopped.
2. Start it again. Expected: the row reads running without a manual reload.
3. Back it up. Expected: a result the row reports.
4. Open a usage panel during an action. Expected: the refresh still runs.

## 6. Updates page (admin)
1. Open `/updates`. Expected: an Outpost row and a row per app.
2. On a dev build. Expected: a one-line note and no update button for Outpost.
3. Fetch `/updates/self`. Expected: 200 JSON.
4. As a non-admin. Expected: `/updates` and `/updates/self` refuse.

## 7. Account
1. Open `/account`. Expected: username, sign-ins and the password form.
2. Change the password with a wrong current one. Expected: an error, password unchanged.

## 8. Not found and wrong method
1. Open an unknown path. Expected: the portal's "Nothing here" page with a link back.
2. GET `/apps`. Expected: 405 with "Use the buttons".

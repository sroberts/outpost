# Push notifications

Outpost and its apps send notifications to people's devices with standard
Web Push (RFC 8030, 8291, 8292), in the Declarative Web Push format. The
app's side is `outpost-service-spec.md` §17; the operator's is the README's
Notifications section. This is why it's built the way it is.

## One subscription per device, on the portal

A push subscription belongs to an origin. Each app has its own host, so the
obvious design, a subscription per app per device, would mean serving a
service worker on every app's host, asking for permission once per app, and,
on iOS, installing every app to the Home Screen separately, because iOS
gives push only to an installed web app.

Instead a device subscribes once, on the portal, and Outpost fans out:

- An app names **users**. `internal/push` looks up their devices, so an app
  never holds a subscription, which is a credential for writing to someone's
  lock screen.
- An app's notifications go only to users who can open it (the gateway's
  `allowed`, the same check as a visit). Each user can mute any app on the
  account page.
- Declarative Web Push opens only a URL on the subscription's origin. An
  app's notification navigates to `/.outpost/push/open?app=…&path=…` on the
  portal, which redirects to that app's own origin and nowhere else.

## A subscription belongs to the sign-in that made it

Each subscription records the sign-in (the portal session's hash) it was
made from. At send time, a device whose sign-in is no longer live is
forgotten, not sent to. So signing a lost phone out on the account page,
**Sign out everywhere**, a password change, or removing the user all stop
notifications, through the same paths that already end sessions, with no
cascade to keep in step.

On iOS a Home Screen app has its own cookie jar, so it is its own sign-in
and its own device. A new sign-in on the same browser gets the same
endpoint from the browser; `push.js` hands it over again on any page where
the server doesn't show this sign-in as having a device.

## The daemon only POSTs to push services

The endpoint comes from the browser, by way of a signed-in user, and the
daemon POSTs to it. Without a check, any user could point the daemon at
something on its own network. Endpoints are accepted only on https, the
default port, and these hosts: `*.push.apple.com`, `fcm.googleapis.com`,
`updates.push.services.mozilla.com` and `*.notify.windows.com`. The client
doesn't follow redirects.

## Crypto from the standard library

The whole of Web Push is one P-256 ECDH, a few HKDFs, an AES-128-GCM seal
(RFC 8291) and an ES256-signed JWT (RFC 8292). `webpush.go` does it with
`crypto/ecdh`, `crypto/hkdf` and `crypto/ecdsa`, and the tests reproduce
RFC 8291's worked example byte for byte, so the key schedule is checked
against the standard and not only against itself. No dependency to audit,
and nothing that could pull in cgo.

## Keys

The VAPID key is in `push.db` (0600), made on first open, beside the
subscriptions that depend on it, as `mail.db` keeps its own token key. An
operator never sets or reads it, so it has no place among the secrets.
Changing it strands every subscription, since each was made against the
public key, so it is never rotated automatically; losing `push.db` means
everyone turns notifications on again.

App tokens are HMACs of the app's name under a key in `push.db`, as for AI
and mail, so an app gets the same token on every start.

The VAPID contact (`sub`) is `[push] contact`, or the portal's own https
address. Apple refuses anything that isn't a `mailto:` or `https:` URI, so
with an http portal and no contact, push stays off and the daemon says so.

## Delivery

`Send` checks the notification, builds one payload, and queues one delivery
per device on an in-memory queue. Four workers deliver, each sealing the
payload afresh, and act on the push service's answer:

| Status | Action |
|---|---|
| 2xx | record the success, reset the failure count |
| 404, 410 | forget the subscription |
| 429, 5xx, no answer | try again, honouring Retry-After, up to 3 tries in all |
| 400, 401, 403 | log: the key or contact is wrong, not the device |
| 413 | log: too large (the payload is capped before sending, so this shouldn't happen) |

The queue isn't persisted. A notification is for now, a restart is brief,
and a push service already holds what it accepted for a device that's off.

## Outpost's own notifications

Admins are told when the supervisor halts an app: the one failure that won't
put itself right. A watcher in `cmd/outpost` reads `Changed()` and `List()`
through the supervisor's public API, as any client does, and notifies on a
service newly halted. An app already halted when the daemon starts isn't
news.

## Not yet

- An admin switch to stop an app from sending at all; today each user
  mutes it.
- Regenerating an app's push token from its Manage menu, as for mail.
- `mutable: true` payloads, for a service worker to change content on the
  device (end-to-end encrypted bodies). Nothing needs it yet.
- Per-category preferences within an app.

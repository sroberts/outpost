# Raw TCP forwards

Built in PR #111. The operator-facing description is in the README under
[Raw TCP forwards](../../README.md#raw-tcp-forwards), and the service's side
is in [`outpost-service-spec.md`](../../outpost-service-spec.md) §1. This note
records why it is shaped the way it is.

## The problem

A service's tailnet node proxied HTTP and nothing else, and the service
contract allowed one port, HTTP. A service whose main interface is something
else, such as a git server that people use over SSH, could put only its web
side on the tailnet. The workarounds were to bind the extra port to the
tailnet address, which breaks the loopback rule, or to run `tailscale serve`
next to Outpost, which Outpost doesn't know about.

## What was built

- A manifest field, `tcp = { <tailnet port> = <loopback port> }`. The node
  listens on each tailnet port and copies bytes to `127.0.0.1:<port>`,
  half-closing each direction as it ends.
- With the gateway on, every connection is checked before the service is
  dialled (`internal/tsnet/forward.go`). The node asks Tailscale who is calling
  (`WhoIs`), then asks the gateway whether that login may open the app
  (`POST /.outpost/tailnet-access`, `internal/gateway/tailnet.go`).
- `users.tailnet_login` in `auth.db`, set with `outpost user tailnet`.

## Decisions

**Gate on tailnet identity rather than not at all.** The gateway's login is a
browser redirect and a cookie, and neither can sit in front of a byte stream.
Without a gate, a forward would be the one way into a gated installation that
skips the login. Tailscale already knows which login every device belongs to,
so the node can say who is calling without the client doing anything.

**The gateway decides, not the node.** The gateway already holds users, the
disabled flag and each app's `allow` list, and it records denials. Making the
decision there means a forward meets the same rules as an HTTP request, and
the node, which runs in its own process for crash isolation, never opens
`auth.db`. The cost is one loopback request per connection. That is cheap for
SSH-like traffic and keeps revocation immediate. Nothing is cached.

**Login to user: an explicit link, then email.** Tailscale logins are often an
email address, but logins such as `alice@github` aren't, so neither rule
covers everyone on its own. An explicit link wins. The email fallback applies
only to users without a link, because a link says which login is theirs, and
only when one such user has that email, so a shared address is refused rather
than guessed. Links are unique, enforced by a partial index.

**Fail closed.** An unknown login, a device that belongs to tags rather than
a person, a WhoIs error and a gateway that can't be reached all refuse the
connection. A node told to gate without a secret refuses to start.

**A per-run secret on loopback.** The gateway's listen address may be public,
and the `Host` header is the client's choice, so the endpoint requires a
loopback remote address and a secret that the daemon generates at start and
hands to the gateway and to each gated node (through the environment, not the
arguments). Without it the endpoint is a 404, so no process on the machine
can find out whose logins are linked.

**No identity reaches the service.** There is no header to put one in.
PROXY protocol v2 could carry it, but no target service reads it, so it was
left out. The service contract says to authenticate on these ports and not to
trust the source address, which is always loopback.

**Never to the HTTP port.** A forward to the service's own `port` would hand
the app requests that never passed the gateway and carry no identity headers.
Validation rejects it, along with tailnet ports 80 and 443 (the node's own)
and forwards that point at another service's ports.

**Gateway off means the tailnet is the boundary**, as it already was for HTTP.
The log line written when the node comes up says so.

## Not done

- An open connection outlives its user being disabled; the check is made when
  the connection opens.
- `outpost user list` doesn't show the link: an existing test pins that
  table's header exactly.
- No TCP healthcheck mode. A service with only a TCP interface still needs an
  HTTP port for `/up`.
- The WhoIs path has been tested only against fakes; see the PR's test plan.

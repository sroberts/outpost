# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

- **The operator, almost always.** The person who runs the Outpost daemon on
  their own machine (a Mac or a Linux box). Comfortable with TOML
  manifests, a CLI and a terminal dashboard. Manages services and users from
  the CLI, and uses the portal, mostly from a phone, as their own front door
  to their apps.
- **Occasionally, other accounts.** People the operator gives an account to.
  The portal works for them, but they don't drive its design.

## Product Purpose

Outpost is a small Go daemon that runs a declared set of local services as
long-lived processes, supervises them, and makes them reachable: on loopback,
on a tailnet through a per-service Tailscale identity, or through its own login
gateway. It replaces Basecamp's ONCE for a personal stack, without Docker
Desktop or a platform-specific supervision layer.

The web surface is the gateway's portal: sign in once, see a catalog that
answers "what can I use here?" at a glance, and reach every app you're allowed
to use with no further prompts. Each app is told who is calling and never has
to implement login itself.

Success is the operator's own stack running unattended, and the operator
getting from the portal to an app in one tap, seeing at once what's down and
why.

## Positioning

Outpost is a personal tool first. The operator's own setup decides what gets
built; others may run it, but it is not shaped as a product for a market.

What it can say that its neighbours can't: one static binary is the daemon, the
CLI, the TUI and the login gateway together, with no container runtime, no
reverse-proxy stack to assemble, and no external identity provider. The
manifest is the source of truth, and the same binary moves from macOS to Linux
without a rewrite.

## Operating Context

- Managing Outpost happens in the CLI and the TUI (`outpost tui`). The portal
  is for signing in and opening apps, plus a Manage menu on each app for
  admins: start, stop, restart, back up now, and update to its latest release,
  since that's what an operator away from a terminal most needs. Users, groups
  and access are managed with `outpost user ...`.
- The portal is reached over a home network or a tailnet, usually behind TLS
  terminated by the gateway itself, `tailscale serve`, or a proxy such as
  Caddy. It is never exposed to the public internet.
- Most portal visits come from phones.
- Apps live at `<name>.<domain>`; the portal lives at `public_url`. The gateway
  owns `/.outpost/` on every app host.

## Capabilities and Constraints

- **Built today:** process supervision with restart policies and backoff,
  SQLite state, the CLI and TUI, autostart under launchd and systemd,
  backup/restore, users (argon2id passwords, sessions), and the login gateway
  with setup, login, logout, single sign-on across app hosts, identity headers
  and an Ed25519-signed identity token; the app catalog (design PR 3), with
  per-app `allow` rules, `public_paths`, `subdomain`/`hostname`, `hidden`, an
  account page and an access-denied page; an update page and a per-app Manage
  menu for admins; raw TCP forwards (`tcp`) on a service's tailnet node, for
  protocols such as SSH, admitted by the caller's tailnet login under the
  same `allow` rules.
- **Not built:** app icons; the station marks each app.
- **Ships in the binary.** Portal pages are Go `html/template` files and CSS
  embedded in the single static binary: no JavaScript build step, no CDN, no
  external fonts or assets. Pages must work offline on a LAN.
- **Strict page headers.** Gateway pages are served with
  `Content-Security-Policy: default-src 'self'; frame-ancestors 'none';
  base-uri 'none'`. Inline scripts and styles, and anything off-host, are
  blocked.
- **Stay pure Go, no cgo.** Nothing that needs a toolchain on the target
  machine.
- **Terminology:** service (a declared, supervised process), manifest (its
  TOML file), app (a service as people reach it through the gateway), portal,
  catalog, operator.
- **Undecided:** how icon files are served to the portal, an open question
  on the design.

## Brand Commitments

- The name is **Outpost**.
- `docs/outpost-header.jpg` is the existing brand asset and should be kept and
  reused.

## Evidence on Hand

- `docs/outpost-header.jpg`, the header image.
- The current portal templates (`internal/gateway/templates/`) and stylesheet
  (`internal/gateway/static/portal.css`), a functional baseline rather than a
  committed visual design.
- Real services to show in a catalog: `yarr` (an RSS reader) runs under
  Outpost today.
- No users beyond the operator and an occasional account, no testimonials, no metrics. Don't
  invent them.

## Product Principles

1. **The manifest is the truth.** Anything the portal shows about an app comes
   from its manifest or from what the daemon observes, never from a second
   place to edit.
2. **Get people to their app.** The portal is a front door, not a destination:
   the fewest steps from sign-in to the app.
3. **Management stays in the terminal, with one exception.** Admins get a
   Manage menu on each app in the portal (start, stop, restart, back up now,
   update), and each goes through the same API as the CLI, an update's
   rollback included. The web surface doesn't otherwise grow admin controls;
   the CLI and TUI own operating Outpost.
4. **Nothing to install, nothing to fetch.** Every page works from the one
   binary on a network with no internet access.
5. **Identity is never the client's to choose.** No UI path may let a request
   decide who a service thinks is calling.

## Accessibility & Inclusion

The portal is held to WCAG 2.2 AA: full keyboard use, visible focus, sufficient
contrast, and screen-reader-correct structure and labels. Phone-sized screens
and touch targets are the primary case, not an adaptation.

# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

- **The operator.** The person who runs the Outpost daemon on their own machine
  (a Mac Mini now, a Linux box later). Comfortable with TOML manifests, a CLI
  and a terminal dashboard. Manages services and users from the CLI, and uses
  the portal like anyone else.
- **A few technical friends.** People the operator gives an account to. They
  are at home with self-hosted tools and need no hand-holding; their job on the
  portal is to sign in once and open the app they came for.

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

Success is the operator's own stack running unattended, and the people they
share it with getting from the portal to an app without friction.

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
  is for signing in and opening apps; there is no admin web UI. Users, groups
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
  and an Ed25519-signed identity token.
- **Planned (design PR 3, `docs/design/gateway-users-catalog.md`):** the app
  catalog on the portal, driven by new optional manifest fields (`title`,
  `description`, `icon`, `subdomain`, `hostname`, `allow`, `hidden`,
  `public_paths`); an account page; an access-denied page. One card per app the
  user may open, showing an icon or lettermark, title, description, address
  and status (running, starting, or unavailable with a one-line reason);
  running first, then alphabetical; a search box only past about a dozen apps,
  as the only JavaScript, with the page working without it. Until then every
  user can open every app, and the portal is a bare list of links.
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
- **Undecided:** how icon files are served to the portal, and what a
  publisher-set `public_paths` may cover; both are open questions on the
  design.

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
- No users beyond the operator and friends, no testimonials, no metrics. Don't
  invent them.

## Product Principles

1. **The manifest is the truth.** Anything the portal shows about an app comes
   from its manifest or from what the daemon observes, never from a second
   place to edit.
2. **Get people to their app.** The portal is a front door, not a destination:
   the fewest steps from sign-in to the app.
3. **Management stays in the terminal.** The web surface doesn't grow admin
   controls; the CLI and TUI own operating Outpost.
4. **Nothing to install, nothing to fetch.** Every page works from the one
   binary on a network with no internet access.
5. **Identity is never the client's to choose.** No UI path may let a request
   decide who a service thinks is calling.

## Accessibility & Inclusion

The portal is held to WCAG 2.2 AA: full keyboard use, visible focus, sufficient
contrast, and screen-reader-correct structure and labels. Phone-sized screens
and touch targets are the primary case, not an adaptation.

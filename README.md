<p align="center">
  <img src="docs/outpost-header.jpg" alt="Outpost — supervise your own services" width="100%">
</p>

# Outpost

A small Go daemon that runs a declared set of local services as long-lived
processes, supervises them, and gives you a CLI to manage them.

The manifest is the source of truth: you declare services as TOML files, and
`outpost` reconciles what is running to match. Process supervision is Outpost's
own code — `launchd` is used only to keep the Outpost daemon itself alive.

Design rationale, and the parts not built yet, live in [`spec.md`](spec.md).
See [Deviations from the spec](#deviations-from-the-spec) for where this code
and that document disagree.

## Status

Built and tested: manifest loading and validation, process supervision with
restart policies and backoff, SQLite state, the CLI, the daemon and its control
socket, autostart under launchd and systemd, backup/restore, the dashboard,
[updating a service](#updating-a-service) from its GitHub releases, users, and
the [login gateway](#the-login-gateway) with its [app catalog](#the-catalog)
and per-app access rules.

Not built: container services (`runtime = "container"` is rejected at apply
time), Hermes `SKILL.md` generation, and app icons in the catalog.

**Tailscale integration has been checked against a real tailnet,** on macOS
with interactive logins, alone and behind the login gateway; see
[Tailscale](#tailscale). New installs
start with it off; see [Configuration](#configuration).

## Building

Go 1.26 or newer. No cgo — the SQLite driver is pure Go, so the result is a
static binary.

```sh
go build -o outpost ./cmd/outpost
go test ./...
```

The binary is ~36 MB, most of which is the `tailscale.com` dependency.

## Quick start

```sh
mkdir -p ~/.outpost/services
cat > ~/.outpost/services/plumbline.toml <<'EOF'
binary  = "/opt/outpost/bin/plumbline"
args    = ["serve", "--port", "8081"]
port    = 8081
storage = "~/.outpost/data/plumbline"
tailscale_name = "plumbline"
EOF

outpost apply     # validate the manifests
outpost serve     # run the daemon (foreground)
```

Then, from another shell:

```sh
outpost list
outpost status plumbline
outpost logs plumbline -f
```

`outpost serve` runs in the foreground and stops every service it started when
it exits, so nothing is left orphaned holding a port. To keep it running across
logins and reboots, see [Running at startup](#running-at-startup).

## Adding and removing services

`outpost add` writes the manifest you would otherwise hand-write, allocating a
free port and filling in the defaults:

```sh
outpost add plumbline --binary /opt/outpost/bin/plumbline \
    --args "serve,--port,8081" --port 8081
outpost add plumbline --binary ... --print   # render it without writing
```

Or install from a GitHub release, which needs no local build:

```sh
outpost add https://github.com/sroberts/plumbline
outpost add sroberts/plumbline --name pl     # shorthand, with a chosen name
```

This fetches the latest release, picks the asset matching your platform —
unpacking a `.tar.gz` or `.zip` if that is how the release ships, and
understanding the usual spellings like `macos` and `x86_64` — verifies it
against the release's checksums when it publishes any (and refuses to install
on a mismatch), and installs it under `~/.outpost/bin/`. If the
repository ships an `outpost.toml`, its declared args, healthcheck and so on
become the manifest's defaults — see [outpost-service-spec.md](outpost-service-spec.md)
§14 for what a service can declare. Flags you pass override it.

Outpost never builds from source; it only ever unpacks what the release
published. A repository with no usable release gets a clear error pointing back
at `--binary`. Private repositories work if you have
the `gh` CLI authenticated or `GITHUB_TOKEN` set; Outpost stores no credentials
of its own.

It validates the result against every other declared service *before* writing,
so a port or name collision is an error rather than something the next `apply`
discovers.

```sh
outpost remove plumbline            # delete the manifest, keep the data
outpost remove plumbline --purge    # delete its storage and backups too
```

A running daemon stops a removed service on its next reconcile, because it is no
longer declared. `--purge` asks for confirmation unless given `--yes`.

## Updating a service

`outpost add <github-url>` records where the binary came from, as `source` and
`release` in the manifest, and `outpost update` installs a later release from
there:

```sh
outpost update --check                 # every service: what is installed, what is latest
outpost update plumbline --check       # one service; changes nothing
outpost update plumbline               # install the latest release
outpost update plumbline --to v1.4.0   # a particular release, which is also how to go back
outpost update --all                   # every service with a newer release, one at a time
```

An update:

1. backs up the service's storage, because a new release may migrate its data
   on first start (`--no-backup` skips it);
2. downloads and verifies the release exactly as `add` does, keeping the binary
   it replaces as `<binary>.previous`;
3. records the new `release` in the manifest, changing only that line;
4. restarts the service and waits for its healthcheck to pass twice in a row,
   for up to 30 seconds plus `start_grace_seconds` (or `--wait`).

**If the new release does not come up healthy, the old binary and manifest are
put back** and the service is restarted on them. Data is not rolled back: if
the new release changed it before failing, `outpost restore` puts back the
backup from step 1.

Update never goes back to an older release unless you name it with `--to`.
GitHub's "latest" skips prereleases, so after `--to v2.0.0-rc1` the latest is
older than what is installed; `outpost update` says so and changes nothing.
Tags are compared as semantic versions, with or without a leading `v`, so
`1.2.0` and `v1.2.0` are the same release.

If the binary has gone missing there is nothing to roll back to, so update asks
for `--force`, and with it puts the binary back from the release.

Only the binary changes. Args, env and the rest of the manifest are yours by
now, so if a release's `outpost.toml` asks for something new, compare it with
`outpost add <url> --print` and edit the manifest yourself. Update only ever
replaces a binary under `~/.outpost/bin/`, never one declared with `--binary`
from somewhere else. A service you stopped gets the new binary and stays
stopped; one the supervisor halted after a crash loop is restarted, since an
update is a likely fix. With no daemon running, the binary and manifest are
updated and nothing is restarted, backed up or checked.

A service added before Outpost recorded sources, or whose binary you put in
`~/.outpost/bin/` yourself, has no `source`. Name it once and it is recorded:

```sh
outpost update yarr --from github.com/sroberts/yarr
```

A release that has just been published may not have its binaries attached
yet, while its build uploads them; update says so and changes nothing. Nothing
updates on its own: automatic updates remain out of scope (spec.md §1).

### Knowing when there's one to install

The daemon asks GitHub about each service with a `source` once a day, and again
when the installed release changes, and remembers the answer in memory. It is
one request per service, and it sends the last `ETag`, so with a token (`gh auth
login`, or `GITHUB_TOKEN`) an unchanged answer doesn't count against GitHub's
rate limit; without one each still counts, out of 60 an hour. A failed check
keeps the last answer, and it and a release still uploading its build are tried
again an hour later. It only finds things out, and there is no setting to turn
it off: on a machine with no route to GitHub, all that happens is a line in the
log each hour.

`outpost list` adds an `UPDATE` column when something is waiting, as its last
column, `outpost status <service>` says what's installed and what's available,
and `outpost tui` shows both. The catalog counts the same answers.

### Updating everything

`outpost update --all` runs the same update for each service with a newer
release, in name order, and **stops at the first that fails**, including one
that rolled back, since that's a reason to look before anything else changes. It
finishes by saying what was updated, what failed and what wasn't tried, and
exits non-zero on a failure.

It asks GitHub afresh rather than trusting the daemon's answer. A service it
can't ask about is reported and makes the exit non-zero, but doesn't stop the
others. One with no `source`, or one update wouldn't touch, is noted and
skipped. `--no-backup` and `--wait` apply to each service; `--to`, `--from` and
`--force` are about one service and are refused, as is naming one. `outpost
update --all --check` is the same list as `outpost update --check`, and changes
nothing.

### From the portal

With the [login gateway](#the-login-gateway) on, an admin can do the same from
a phone. The catalog links to `/updates`, and says when an update is waiting.
The page shows each app installed from a release, what it runs and what's
available, with a button to update.

That button runs `outpost update <service>` inside the daemon, over its own
socket: the same backup, lock, health wait and rollback, and the same manifest
edit. It only installs the latest release of the `source` already in the
manifest. `--to`, `--from`, `--force` and `--no-backup` stay on the CLI, so the
portal can't go back to an older release or change where an app comes from.

The page shows what the daemon's daily check found. Opening the portal also
checks for new releases at most once an hour, in the background, at one GitHub
request per app, and **Check now** asks straight away. Nothing installs until an admin presses the button. The result of the last
update stays on the page until the daemon restarts, and `outpost gateway
activity` records which admin started each one and how it ended.

Restarting the daemon during an update ends it where it stands, with no
rollback, just as killing `outpost update` would. Wait for it to finish, or
check `outpost status <service>` afterwards.

## The service contract

Outpost does not care what a service does internally. It requires that the
service:

- listens on one TCP port, given as `port`
- serves a healthcheck at `healthcheck` (default `/up`) returning 2xx when healthy
- takes configuration from environment variables or CLI arguments
- writes persistent state under one directory, handed to it as `OUTPOST_STORAGE_DIR`
- exits non-zero on unrecoverable failure

Outpost injects the manifest's `env` plus `OUTPOST_STORAGE_DIR` into the
process environment. `OUTPOST_STORAGE_DIR` is appended last, so a manifest
cannot accidentally override it.

## Manifest schema

One TOML file per service in `~/.outpost/services/`. The filename minus its
extension is the service name unless `name` overrides it.

```toml
# ~/.outpost/services/plumbline.toml
name           = "plumbline"        # optional; defaults to the filename
runtime        = "process"          # "process" | "container"

# runtime = "process"
binary         = "/opt/outpost/bin/plumbline"
args           = ["serve", "--port", "8081"]
env            = { LOG_LEVEL = "info" }
source         = "sroberts/plumbline"          # written by `outpost add <url>`
release        = "v1.3.0"                      # rewritten by `outpost update`

# runtime = "container" — not implemented yet, rejected at apply time
# image          = "koreader/kosync:latest"
# container_args = []

port           = 8081                          # required
storage        = "~/.outpost/data/plumbline"   # required, created 0700 if absent
tailscale_name = "plumbline"                   # required, unique on the tailnet
funnel         = false                         # rejected if true
healthcheck    = "/up"
mcp            = true                          # informational only
hermes_skill   = true                          # informational until §11 is built
restart_policy = "on-failure"                  # "on-failure" | "always" | "never"
backoff_seconds     = 5
start_grace_seconds = 30
```

| Field | Default | Notes |
|---|---|---|
| `name` | filename | Must be unique |
| `runtime` | `process` | The only valid value; Outpost runs native processes |
| `port` | — | Required; must be unique across `process` services |
| `storage` | — | Required; `~` is expanded, created `0700` if missing |
| `tailscale_name` | — | Required unless `tailscale.enabled = false`; must be unique |
| `healthcheck` | `/up` | Must start with `/` |
| `restart_policy` | `on-failure` | See below |
| `backoff_seconds` | `5` | Doubles per consecutive restart, capped at 300s |
| `start_grace_seconds` | `30` | Healthcheck failures do not count during this window |
| `funnel` | `false` | `true` is rejected — deferred, not designed out |
| `source` | — | GitHub `owner/repo` the binary came from; what `outpost update` fetches |
| `release` | — | Release tag installed. Neither field changes what runs, so editing them restarts nothing |

`outpost apply` validates the whole set and fails loudly rather than
half-applying. It checks for duplicate names, ports and Tailscale names,
missing required fields, and runtime/field mismatches.

### Restart policies

| Policy | Behaviour |
|---|---|
| `on-failure` | Restart on a non-zero exit or a failed healthcheck |
| `always` | Also restart after a clean (zero) exit |
| `never` | Never restart automatically |

Backoff starts at `backoff_seconds`, doubles each consecutive restart, and caps
at 300s. It resets once a service has run healthily for 60 seconds, so a service
that crashes once an hour does not creep toward the cap.

A service the supervisor gives up on — `restart_policy = "never"`, or a health
failure under that policy — is marked **halted** and shows as such in
`outpost status`. Only an explicit `outpost start` or `outpost restart` resumes
it; a routine status update will not clear the halt.

### Startup grace

A service is not penalised for failing its healthcheck during the first
`start_grace_seconds` after it starts, and reports status `starting`. The window
ends at the first successful healthcheck, or when it expires. This exists so
that anything slow to bind its port — a database, a JVM service, anything
running migrations on boot — is not put into a permanent restart loop.

A crash during the grace window is still a crash, and is restarted per policy.
The grace covers slow healthchecks, not a process that exits.

## CLI

```
outpost serve [--tailnet=BOOL]   run the daemon (reconcile loop + control socket)
outpost add <name> --binary ...  declare a new service from a local binary
outpost add <github-url>         install from the latest GitHub release
outpost update <service>         install a newer release, rolling back if it fails
                                 its healthcheck (--check, --to TAG, --from REPO)
outpost update --all             update every service with a newer release, stopping
                                 at the first that fails (--check lists them)
outpost remove <name> [--purge]  undeclare a service, optionally deleting its data
outpost apply                    validate manifests and reconcile
outpost list                     table of services and their status
outpost status <service>         health, restart count and recent events
outpost logs <service> [-f]      show or follow a service's logs
outpost logs <service> --events  show the service's recent activity
outpost start <service>          start a stopped service
outpost stop <service>           stop a running service
outpost restart <service>        restart a service
outpost backup [service]         archive a service's storage (all if omitted)
outpost backup --list <service>  list existing backups
outpost restore <service> [ts]   restore from a backup (newest if ts omitted)
outpost tui                      launch the dashboard
outpost config [--init]          show the global configuration, or write a starter
outpost config --tailscale on|off
                                 turn per-service Tailscale identities on or off
outpost version                  print the build identity of this binary
outpost install [--system]       install the launchd/systemd autostart service
outpost uninstall [--system]     stop that service and remove it
```

Users log in through the [login gateway](#the-login-gateway). They're managed
from the CLI, through the daemon:

```
outpost user add <name> [--admin] [--group G] [--email E] [--display-name N]
                                 add a user; prompts for a password, or takes
                                 --password-stdin or --no-password
outpost user passwd <name>       set a password and end the user's sessions
outpost user rm <name>           remove a user
outpost user list                users, their groups and live sessions
outpost user groups <name> +g -g add to and remove from groups
outpost user disable|enable <name>
outpost user logout <name>       end every session the user has
outpost auth rotate-key          replace the key that signs identity tokens
outpost gateway status           the portal address and the apps it routes
outpost gateway activity [-n N]  sign-ins, failures and refusals at the gateway
```

Passwords are typed at a prompt with no echo, never on the command line, and
cross only the owner-only control socket. Removing, disabling or demoting the
last enabled admin is refused.

Global flags:

| Flag | Default | Notes |
|---|---|---|
| `--home <dir>` | `$OUTPOST_HOME`, else `~/.outpost` | Outpost root |
| `-h`, `--help` | | Show usage |

`serve` also takes `--interval` (default 3s), `--tailnet`, and
`--tailnet-verbose`. Flags may appear on either side of a service name:
`outpost status --events 20 web` and `outpost status web --events 20` both work.

Every command except `logs` talks to the running daemon over the control
socket, so the CLI and the daemon share one code path. `logs` reads the log file
directly — it is a local file, and streaming a follow over the socket would need
a framed protocol for no practical gain.

Commands that need the daemon fail with a clear error when none is running.
`apply` still validates manifests without one, since that is a pure function of
the files on disk.

## Dashboard

`outpost tui` is a Bubble Tea dashboard over the same control socket the CLI
uses — it goes through the daemon, not the state store, so there is one code
path to the supervisor.

```
  Outpost                                                        3 services

    NAME       RUNTIME  STATUS             UPTIME  RESTARTS
  ▸ plumbline  process  ● running          3h      0
    kosync     process  ▲ unhealthy        -       3
    broken     process  ✖ failed (halted)  -       12

  plumbline ──────────────────────────────────────────────────────────────
  port 8081 · /up · on-failure, backoff 5s
  tailscale plumbline
  storage   ~/.outpost/data/plumbline
  instance  4242

  recent activity
  16:33:29  health-ok    healthcheck recovered
  13:34:29  started      started (id 4242)

  ↑/k up • ↓/j down • s start • x stop • r restart • l logs • ? help • q quit
```

| Key | Action |
|---|---|
| `↑`/`k`, `↓`/`j` | move between services |
| `s`, `x`, `r` | start, stop, restart the selected service |
| `l` | toggle a log pane for the selected service |
| `R` | refresh now |
| `?` | expand the help |
| `q`, `esc`, `ctrl+c` | quit |

It refreshes every two seconds, and an action refreshes immediately rather than
waiting for the next tick. Status is carried by both colour and a glyph, so it
reads correctly without colour. With no daemon running it says so and explains
how to start one, then reconnects on its own once one appears.

## Configuration

Global settings live in `~/.outpost/config.toml`. Everything per-service belongs
in that service's manifest instead. The file is optional — Outpost works with no
configuration at all — and `outpost config --init` writes a commented starter.

```toml
[tailscale]
enabled  = false
# auth_key = "tskey-..."
```

| Setting | Default | Meaning |
|---|---|---|
| `tailscale.enabled` | see below | Whether services get a Tailscale identity |
| `tailscale.auth_key` | — | Registers nodes unattended; `TS_AUTHKEY` wins over it |

New installs start with Tailscale off: the first command to create
`~/.outpost` writes `enabled = false`, and `outpost install` asks (see
[Running at startup](#running-at-startup)). A home with no setting at all keeps
running with it on, since that was the default before, and existing installs
shouldn't lose their tailnet on upgrade. `outpost config --tailscale on|off`
changes it at any time; restart the daemon afterwards.

`[gateway]` turns on the login gateway; its settings are in
[The login gateway](#the-login-gateway).

The daemon reads the file only when it starts. Manifests reload by themselves,
but a change here waits for a restart, so `outpost list`, `status`, `config`
and `gateway status` say when the file no longer matches what the daemon is
running. Comments and layout don't count as a change.

The file may hold an auth key, so it is written `0600`. Prefer `TS_AUTHKEY` in
the environment, or `outpost install --authkey`, over writing one to disk.

## Running without Tailscale

Tailscale is off for new installs and never required. To turn it off on an
existing install, run `outpost config --tailscale off`, or set:

```toml
[tailscale]
enabled = false
```

Then `tailscale_name` is no longer required in a manifest, `outpost add` stops
generating one, and the daemon never contacts Tailscale. Everything else —
supervision, health checks, restarts, backups, the dashboard — is unchanged, and
`outpost list` simply shows `-` in the tailnet column.

`outpost serve --tailnet=false` does the same for a single run without editing
the config; `--tailnet` forces it on.

Services still bind loopback, because that is what the service contract asks
for. Without Tailscale they are reachable from the machine itself; exposing them
further is between you and whatever you put in front of them — Outpost has no
`bind` setting, because it never binds the port itself.

## The login gateway

An optional HTTP front door in the daemon. People log in once at a portal and
reach each service at its own subdomain, `<name>.<domain>`, and every request
the service receives says who sent it. It's off unless you turn it on:

```toml
[gateway]
enabled    = true
public_url = "https://outpost.example"   # the portal, as browsers see it
# name            = "Home Outpost"       # what the portal calls itself; defaults to Outpost
# listen          = "127.0.0.1:8443"     # where the daemon listens
# domain          = "outpost.example"    # apps at <name>.<domain>; defaults to public_url's host
# tls_cert        = "/path/to/fullchain.pem"   # serve TLS directly; leave both unset
# tls_key         = "/path/to/privkey.pem"     # when something in front terminates it
# trusted_proxies = ["127.0.0.1/32", "::1/128"] # may set X-Forwarded-*
# allow_insecure  = false                # see below
```

With Tailscale on, that is all the setup it needs: each app is served at its
own node's tailnet address (below), with that node's certificate, and the
portal can go on the tailnet too, with
`tailscale serve --bg http://127.0.0.1:8443` and a `public_url` of
`https://<machine>.<tailnet>.ts.net`. Without Tailscale, it needs a wildcard:
`*.outpost.example` resolving to the machine, and a certificate that covers
it, whether the gateway serves TLS itself or something in front of it does. A plain-http `public_url` is fine on loopback, behind a
proxy that terminates TLS, but with a network-facing `listen` Outpost rejects
the config unless `allow_insecure = true`, because passwords would cross the
network in clear.

With no users yet, `outpost serve` logs a one-time setup link that creates the
first admin; `outpost user add <name> --admin` does the same from a terminal.
The link stops working once anyone exists, or when the daemon restarts.

What a service receives on every request, once its user is logged in:

| Header | Example |
|---|---|
| `X-Outpost-User` | `alice` |
| `X-Outpost-Name` | `Alice Example` (may be empty) |
| `X-Outpost-Email` | `alice@example.com` (may be empty) |
| `X-Outpost-Groups` | `family,kids` |
| `X-Outpost-Identity` | an Ed25519-signed JWT, valid for 60 seconds, `aud` the app's host |

Any `X-Outpost-*` header a client sends is removed first, and Outpost's own
cookie never reaches the service. The token verifies against the keys at
`/.well-known/jwks.json`, which `outpost auth rotate-key` replaces, keeping the
old key published for a day. With the gateway on, every service also gets
`OUTPOST_AUTH=gateway`, `OUTPOST_ISSUER` and `OUTPOST_JWKS_URL`; a running
service picks them up at its next restart.

With Tailscale on as well, a service's own tailnet address goes through the
gateway too, so there is no way in that skips the login. Its node serves
`https://<tailscale_name>.<tailnet>.ts.net` with the tailnet's certificate,
redirects plain http there, and hands every request to the gateway, which
treats that name as the app's address: the portal links to it and the login
sends you back to it. No wildcard DNS or certificate is needed for this. If
Tailscale gives the node a different name because the one asked for is taken
(`yarr-1`), the gateway uses the name the node actually got. Turning the
gateway on in an existing tailnet setup changes nodes from plain http on port
80 to this; restart the daemon to pick it up.

What the gateway decides goes in the activity feed, and `outpost gateway
activity` shows it: who signed in or out and from which address, failed and
throttled sign-ins, refused apps, password changes, use of the setup link, and
updates started from the portal.
A failure is recorded the first time; repeats of the same one within ten
minutes become a single line, such as
`4 more failed sign-ins for "alice" from 192.0.2.7 since 21:14`. Throttled
attempts are counted by address alone, whatever name they try. A name
someone typed at the sign-in form is shown quoted, with control characters
escaped. `outpost user list` shows each user's last sign-in.

### The catalog

After signing in, the portal lists the apps you can open as stations on one
line, running apps first. An app that isn't up shows why ("Stopped after 3
crashes", "Stopped on purpose"), and an admin also sees the `outpost status`
command to look closer. Past a dozen apps a filter appears. The account page
lists where you're signed in and ends any one of those sessions, such as a lost
phone's, along with the apps it was signed in to. It also changes your password,
which signs out every other device, and signs you out everywhere at once.

What the catalog shows, and who may open an app, come from optional manifest
fields:

```toml
title        = "Yarr"                      # what the catalog calls it; defaults to the name
description  = "RSS reader"                # one line under the title
subdomain    = "rss"                       # serve at rss.<domain> instead of <name>.<domain>
hostname     = "yarr.tail1234.ts.net"      # or a full hostname instead (not both)
allow        = ["alice", "group:family"]   # who may open it; empty means everyone
hidden       = true                        # leave it out of the catalog (still reachable)
public_paths = ["/webhook/*", "/feed.xml"] # served without a login
```

- **`allow`** is checked on every request and before a sign-in link is
  issued; someone not on it gets a plain "No access" page. Admins reach every
  app and see every one, hidden ones marked. A name that matches no user or
  group yet is logged, not refused, so a manifest can come first.
- **`public_paths`** are for webhooks and feeds that carry their own
  credentials. A pattern is an exact path or a prefix ending in `/*`.
  Requests there still have every `X-Outpost-*` header and Outpost's cookie
  removed, and only a path already in its simplest form matches, so
  `/webhook/../admin` gets no exemption.
- A release's `outpost.toml` may set `title`, `description` and
  `public_paths`, which `outpost add` copies into the manifest. Who gets in and
  where the app lives stay the operator's call.
- With the gateway on, a service name that can't be a DNS label is a manifest
  error unless `subdomain` or `hostname` gives it an address.

Not built yet: app icons. The catalog marks each app with its station.

## Layout

Everything lives under `~/.outpost` (or `$OUTPOST_HOME`, or `--home`):

```
~/.outpost/
  services/<name>.toml     service manifests — the source of truth
  data/<name>/             default location for service storage
  logs/<name>.log          combined stdout/stderr, rotated at 10 MiB, 3 kept
  backups/<name>/<ts>.tar.gz
  tsnet/<name>/            per-service Tailscale node identity
  state.db                 SQLite observed state and event log
  auth.db                  users, password hashes, sessions, signing keys (0600)
  config.toml              global settings, mode 0600 (optional)
  outpost.sock             control socket, mode 0600
  outpost.pid              running daemon's PID
  logs/outpost.out.log     the daemon's own output (macOS; journal on Linux)
```

`state.db` is a cache of observed reality, not the source of truth. Deleting it
and re-running `outpost apply` converges to the same running state; there is a
test for exactly that. The one thing it holds that is *not* a cache is your
intent: `outpost stop` records that a service should stay down.

`auth.db` is the opposite: nothing can rebuild it. It holds users, argon2id
password hashes, sessions (stored only as SHA-256 hashes of their tokens) and
the keys that sign identity tokens, so it lives apart from `state.db` and
should be backed up with the manifests. `outpost backup` covers service
storage, not this file.

> **Note on socket paths.** Unix domain sockets have a hard length limit in the
> kernel (104 bytes on macOS). `~/.outpost/outpost.sock` is well under it, but a
> deeply nested `--home` can exceed it; Outpost reports this explicitly rather
> than failing with a bare `bind: invalid argument`.

## Running at startup

Outpost supervises the services; the init system supervises Outpost. Both
platforms are supported, and the generated file is the same shape either way:
start at boot or login, restart the daemon if it dies, and give it enough time
on the way down to stop its services in order.

```sh
outpost install            # write the launchd job / systemd unit
outpost install --enable   # write it and start it
outpost install --print    # print it without writing anything
outpost install --system   # boot-time service instead of per-user (needs root)
outpost uninstall          # stop it and remove it
```

`install` writes the file and prints the commands that would enable it, rather
than running them — registering a background service should be a deliberate act.
Pass `--enable` to have Outpost run them for you.

Pass `--authkey` (or set `TS_AUTHKEY`) to put a Tailscale auth key in the
service environment, which keeps it out of the manifests.

If `config.toml` doesn't yet say whether to use Tailscale, `install` checks
whether Tailscale is running on the machine, says what it found, and asks
whether each service should get its own Tailscale identity. The answer is
written to `config.toml`, so it's asked once. `--tailscale` and
`--no-tailscale` answer without asking, and so does an auth key: `--authkey`
or `TS_AUTHKEY` means on, unless the home already has a setting or
`--no-tailscale` is given. Otherwise, with no terminal, a new install gets
Tailscale off and an existing one is left as it is.

### Upgrading

Replace the binary, then re-run `outpost install` (with the same flags as
before, such as `--system`). That rewrites the service file for the new
binary and asks any question a new version has added. Restart the daemon to
pick up the new binary.

### User scope versus system scope

| | Starts | Needs root | Where |
|---|---|---|---|
| default | at login | no | `~/Library/LaunchAgents`, `~/.config/systemd/user` |
| `--system` | at boot | yes | `/Library/LaunchDaemons`, `/etc/systemd/system` |

**This distinction matters on a headless machine.** The default installs a
per-user service, which starts when you log in — so a Mac Mini that reboots
unattended will not start Outpost until someone logs in. Either enable automatic
login, or install with `--system`.

On Linux the equivalent trap is lingering: a systemd user unit stops at logout
and does not start at boot unless `loginctl enable-linger <user>` has been run.
`outpost install` prints that command along with the others.

### What the generated files do

**macOS (launchd):** `RunAtLoad` and `KeepAlive` start the daemon and bring it
back if it exits. `ThrottleInterval` stops a daemon that fails immediately from
spinning. `ExitTimeOut` gives Outpost 60 seconds to stop its services before
launchd escalates to SIGKILL. Outpost's own output goes to
`~/.outpost/logs/outpost.{out,err}.log`.

**Linux (systemd):** `Restart=always` with `RestartSec=5`, and
`StartLimitIntervalSec=0` so systemd never gives up restarting.
`KillMode=mixed` sends SIGTERM to the daemon alone, so it can stop its services
in order, and `TimeoutStopSec=60` bounds that before systemd kills what is left.
Outpost's own output goes to the journal — `journalctl --user -u outpost -f`.

Per-service logs are files under `~/.outpost/logs/` on both platforms either way.

### Surviving an unclean shutdown

A daemon killed with SIGKILL — an OOM kill, a power cut, `kill -9` — never runs
its shutdown path, so its services keep running, reparented to init and still
holding their ports.

On startup Outpost finds those processes, using the PIDs recorded in `state.db`,
and stops them before starting replacements. Without that, every service would
restart-loop against a port its own orphan still held. A recorded PID is only
acted on if the process is alive *and* its command line matches the manifest's
binary — PIDs get recycled, and killing an unrelated process would be worse than
leaving an orphan.

Orphans are stopped rather than adopted: a process that is not the daemon's own
child cannot be waited on, so the supervisor could not observe it exiting.

## Backups

```sh
outpost backup plumbline          # one service
outpost backup                    # every service, in turn
outpost backup --list plumbline
outpost restore plumbline
outpost restore plumbline 2026-09-08T23-04-11
```

A backup stops the service, tars its storage directory to
`~/.outpost/backups/<service>/<timestamp>.tar.gz`, and restarts it if it was
running. A service you had already stopped stays stopped.

Restore stops the service, replaces its storage from the archive, and restarts
it. **Restore discards anything written since the backup**, including files the
archive does not contain. The previous directory is kept until the new one is
in place, so a failed restore does not destroy the data it was replacing.

Archives are plain `tar.gz` and can be unpacked with standard tools. Extraction
refuses entries that escape the target directory, including absolute paths,
`../` traversal, and writes through a symlink the archive itself created.

### Scheduling

Outpost has no scheduler of its own. It writes one for the init system instead —
a launchd calendar job on macOS, a systemd timer on Linux — so the scheduling is
handled by something already designed for it (spec §10, open question 2).

```sh
outpost backup --schedule daily --at 03:30      # write the job
outpost backup --schedule daily --enable        # write it and activate it
outpost backup --schedule hourly
outpost backup --schedule weekly --at 04:00
outpost backup plumbline --schedule daily       # one service rather than all
outpost backup --unschedule                     # remove it
```

As with `install`, it writes the file and prints the command that activates it
rather than running it; `--enable` opts in, and `--system` makes it a boot-time
job.

The systemd timer is `Persistent=true`, so a backup missed while the machine was
off runs once it comes back. Both jobs are one-shots — nothing restarts them
when the backup finishes.

## Tailscale

**Checked by hand.** On macOS, a node joined a real tailnet through the
interactive login URL and served its service over MagicDNS. Killing the node
left the daemon and the service running, and the node came back without a
new login. Killing the daemon ended the node with it. Then again behind the
login gateway: the node served `https://yarr.<tailnet>.ts.net` with a
certificate Tailscale issued on the first request, which took about half a
minute, redirected plain http there, and sent signed-out requests to the
portal's login. Not yet tried: an auth key, Linux under systemd, and more than
one service at a time.

```sh
TS_AUTHKEY=tskey-... outpost serve --tailnet
```

Each service gets its own `tsnet.Server`, named by `tailscale_name`, with node
state under `~/.outpost/tsnet/<service>/` so identities persist across restarts.
Each node runs in a child process, `outpost __tsnet-node`, so a Tailscale panic
ends that node, which the daemon restarts, rather than the daemon.
The node listens on the tailnet and reverse-proxies to `127.0.0.1:<port>`, so
the supervised process never binds anything but loopback. With the
[login gateway](#the-login-gateway) on, it serves HTTPS and proxies to the
gateway instead, so the tailnet address is behind the login.

Without `TS_AUTHKEY`, tsnet logs a one-time login URL per service on first
run, once each time the node starts; a daemon restart gives a new one. A node gets the name it
asks for only if no other device on the tailnet has it, otherwise Tailscale
appends `-1`. To take a name over from an old device, delete that device in
the Tailscale admin console *before* logging the new node in.
Each service costs one device on your tailnet — check your plan's limit before
this scales past a handful. `funnel = true` is rejected.

Joining the tailnet happens in the background: `Attach` records intent and
returns, so a node that is slow to register — or waiting on a first-run login
URL — never stalls supervision or the control socket. A failed attach is retried
with backoff (2s, doubling to 60s) until it succeeds or the service is detached,
and the reconcile loop re-asserts membership for every running service, so a
service cannot end up silently off the tailnet.

## Development

```sh
go test ./...
go test ./... -race
go vet ./...
```

Layout follows `spec.md` §4. `internal/manifest` parses and validates;
`internal/runtime` starts and stops things; `internal/supervisor` decides what
*should* run and owns the reconcile loop; `internal/state` records what was
observed; `internal/ipc` carries CLI requests to the daemon; `internal/backup`,
`internal/tsnet`, `internal/autostart`, `internal/acquire`, `internal/config`,
`internal/version` and `internal/tui` are self-contained.

The supervisor's state is deliberately single-threaded. Its exported API hands
work to the reconcile loop through a command queue rather than locking, and runs
inline when no loop is running, so every method is safe to call from any
goroutine. `internal/runtime/testdata/fakeservice` is a real HTTP service that
can be told to crash or go unhealthy on demand, used by the runtime tests.

## Deviations from the spec

`spec.md` is the design document and has not been updated as the code was
written. Where the two disagree, this section is current.

**`start_grace_seconds` is new.** Not in spec §5. Added because the reconcile
loop began healthchecking immediately after start, which put any slow-starting
service into a permanent restart loop.

**The state store has three extra columns.** Spec §8 lists `services(name,
runtime, status, pid_or_container_id, last_started_at, restart_count,
updated_at)`. This adds `desired` (your start/stop intent), `restart_epoch` (how
`outpost restart` reaches the daemon) and `halted` (whether the supervisor has
given up). They are load-bearing, not incidental.

**The manifest has `source` and `release`.** Not in spec §5. `outpost add`
writes them when it installs from a release, and `outpost update` reads them.
Neither affects what runs. Updating stays a command you run; spec §1's
deferral of automatic updates still stands.

**There is a web surface.** Spec §1 defers a web UI. The login gateway added a
portal for signing in and opening apps, and the portal has one management
control: an admin can update an app to its latest release. Everything else
stays in the CLI and the TUI.

**The CLI differs from spec §12.** Added: `serve`, `add`, `update`, `remove`,
`install`, `uninstall`, `config`, `version`, `user`, `auth`, `gateway`,
`--home` / `$OUTPOST_HOME`, `logs --events`, `backup --list`,
`backup --schedule`, `--tailnet`. `restore` takes the timestamp as optional,
defaulting to the newest archive. Not built: `outpost hermes publish`, which
spec §12 lists.

**Manifest reloading polls instead of using fsnotify.** Spec §6 calls for
fsnotify. The daemon hashes the manifest directory each tick and re-applies on
change, which avoids a dependency; fsnotify would only reduce reload latency
below one tick.

**Containers were dropped.** Spec §1 has services as "either a native process
or an OCI container", and §9 designs a container runtime. Outpost supervises
native Go processes only, so `image`, `container_args` and `container_engine`
are gone from the schema and `runtime` accepts only `"process"`. Spec §9 and
build step 8 no longer apply.

**Tailscale is opt-in for new installs.** Spec §1 and §7 have every service
on the tailnet, reachable only through Tailscale. New installs now start with
`tailscale.enabled = false`, and `outpost install` offers to turn it on when it
finds Tailscale running. Homes with no setting keep the old behaviour, on.

**There is a small web UI.** Spec §1 rules one out for v1. The login gateway
serves a portal for logging in and listing apps; managing Outpost stays in the
CLI and the TUI. Spec §7 also has tsnet as the only way in; the gateway is
another, and the two can run side by side.

**With the gateway on, tailnet nodes go through it.** Spec §7 has each node
reverse-proxy straight to the service on port 80. With the login gateway on,
a node serves HTTPS with its own tailnet certificate and proxies to the
gateway instead, so the service's tailnet address is behind the login, and
the portal links to that address. Without the gateway, nodes behave as spec
§7 describes.

**`funnel` is rejected, not ignored.** It is accepted by the schema and refused
at apply time with an explicit error, per spec §7.

**Backup scheduling uses the init system.** Spec §10 leaves this open; Outpost
generates a launchd job or a systemd timer rather than growing a scheduler.

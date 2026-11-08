<p align="center">
  <img src="docs/outpost-header.jpg" alt="Outpost — supervise your own services" width="100%">
</p>

# Outpost

A small Go daemon that runs a declared set of local services as long-lived
processes, supervises them, and gives you a CLI to manage them. It is for
people running a handful of their own services on a home machine or small
server, who want them restarted when they fail, backed up, and reachable
over Tailscale or behind one login, without a container stack.

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
make build      # ./outpost, stamped with its version
make install    # into ~/.local/bin (set PREFIX to change it)
make check      # everything CI runs
```

`make build` stamps the version into the binary; a bare `go build` works but
reports a pseudo-version. Tagged releases publish static binaries for macOS
and Linux, on arm64 and amd64, with a `SHA256SUMS` file. Each binary also
carries a build provenance attestation, which
`gh attestation verify outpost-<os>-<arch> --owner sroberts` checks.

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

With the [login gateway](#the-login-gateway) on, an admin can do the same from
the portal: **Add an app** on the catalog opens `/add`, which takes a repository
link in any of the forms above. **Look it up** runs `outpost add --print`
inside the daemon and shows the manifest it would write and the asset it would
install, with nothing downloaded; **Add** then runs `outpost add <owner/repo>`.
The page takes no flags, so the release's `outpost.toml` decides the rest, and
it never passes `--force`, so it can't replace a service that's already
declared. Anything else, including a name clash, stays on the CLI.

```sh
outpost remove plumbline            # delete the manifest, keep the data
outpost remove plumbline --purge    # delete its storage and backups too
```

With the daemon running, `remove` stops the service straight away and clears its
record and history. `--purge` asks for confirmation unless given `--yes`.

Deleting a manifest file by hand works too. The daemon stops the service, but
keeps its record and history, and `outpost list` shows it as `removed` (also
for a manifest deleted while the daemon was down). `outpost start` refuses it.
Put the manifest back to run it again, or clear it with `outpost remove <name>`,
which works without the file. With `--purge` it then deletes the service's
backups, though not its storage: only the manifest said where that was. If the
record names a process that is still alive, left by a daemon that died, `remove`
refuses until you have stopped it, since without the manifest Outpost cannot
tell whether it is the old service.

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
3. records the new `release` in the manifest, and whether it was `verified`,
   changing only those lines;
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

A service whose installed release was checked against its checksums isn't
updated to one that publishes none for its asset: update refuses before
downloading, and `--allow-unverified` installs it anyway.

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
a phone. When an update is waiting, the catalog says so and links to
`/updates`; with nothing to install there's no link, but the page is still there.
The page shows each app installed from a release, what it runs and what's
available, with a button to update. Outpost itself has a row there too: see
[Updating Outpost itself](#updating-outpost-itself).

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
rollback, just as killing `outpost update` would, so `outpost daemon restart`
refuses to while one is running unless given `--force`. Restarted any other way,
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

## Outpost-ready projects

These open-source projects follow the service contract and ship an
`outpost.toml` descriptor, so Outpost knows how to run them. Each is also a
worked example for writing your own.

| Project | Description | Install |
|---|---|---|
| [Domestique](https://github.com/sroberts/domestique) | Training log for cycling and lifting, importing from Whoop, Wahoo, Zwift and Strava | `outpost add sroberts/domestique` |
| [Sous](https://github.com/sroberts/sous) | Household recipe manager with step-by-step cooking and timers | `outpost add sroberts/sous` |
| [Yarr](https://github.com/sroberts/yarr) | RSS reader with Instapaper and swipe triage; a fork of [nkanaev/yarr](https://github.com/nkanaev/yarr) | `outpost add sroberts/yarr` |
| [Soft Serve](https://github.com/sroberts/soft-serve) | Git server you browse and push to over SSH; a fork of [charmbracelet/soft-serve](https://github.com/charmbracelet/soft-serve) | build it, then `outpost add --binary` |

Domestique, Sous and Yarr also serve MCP, so AI agents can use them through
the Outpost portal.

`outpost add` installs a project's latest release, checked against its
`SHA256SUMS`, and writes a manifest from its descriptor. See
[Adding and removing services](#adding-and-removing-services). The Soft Serve
fork publishes no releases yet, so build it and add the binary by path.

Have a project that runs under Outpost? `outpost-service-spec.md` is the
contract it has to meet, and §14 covers publishing releases `outpost add` can
install.

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
funnel         = false                         # answer public_paths from the internet; see Funnel
healthcheck    = "/up"
mcp            = true                          # informational only
mcp_path       = "/mcp"                        # where you serve MCP; see Access tokens
tcp            = { 22 = 23231 }                # raw TCP on the node; see Raw TCP forwards
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
| `funnel` | `false` | `true` answers `public_paths`, and only those, from the internet through Tailscale Funnel. Needs the gateway, a `tailscale_name` and at least one public path. An app's Internet access setting in the portal overrides it; see [Funnel](#funnel-webhooks-from-the-internet) |
| `source` | — | GitHub `owner/repo` the binary came from; what `outpost update` fetches |
| `release` | — | Release tag installed. Neither field changes what runs, so editing them restarts nothing |
| `mcp_path` | — | Where the service serves MCP (Streamable HTTP). Lets MCP clients with an access token reach it at `https://<portal>/mcp/<name>`; see [MCP clients](#mcp-clients) |
| `tcp` | — | Tailnet port to loopback port, for protocols that aren't HTTP, such as SSH. Needs a `tailscale_name`; not 80 or 443, and never the service's own `port`; see [Raw TCP forwards](#raw-tcp-forwards) |

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
outpost update --self            update Outpost itself and restart the daemon on it,
                                 rolling back if it doesn't come up (--check, --to TAG)
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
outpost config --authkey         save a Tailscale auth key, read from the terminal
                                 or stdin (--no-authkey removes it)
outpost version                  print the build identity of this binary
outpost install [--system]       install the launchd/systemd autostart service
outpost uninstall [--system]     stop that service and remove it
outpost daemon restart           restart the daemon through that service and wait
                                 for it to come back (--force, --wait D)
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
outpost user sessions <name>     where the user is signed in
outpost user logout <name>       end every session and access token the user has
outpost user tailnet <name> <login>|off
                                 link a Tailscale login to the user, for tcp forwards
outpost token create <user> [--name N] [--app A]... [--expires 90d]
                                 make an access token; prints the secret once
outpost token list [<user>]      access tokens: scope, expiry and last use
outpost token revoke <id>        end an access token
outpost auth rotate-key          replace the key that signs identity tokens
outpost gateway status           the portal address and the apps it routes
outpost gateway activity [-n N]  sign-ins, failures and refusals at the gateway
```

[Secrets](#secrets) are managed the same way:

```
outpost secret set <name> [--env VAR] [--service S]... [--global] [--value-stdin]
                                 create a secret, or rotate its value; prompts for
                                 it with no echo, or reads all of stdin
outpost secret list              secrets, their variables and who gets them
outpost secret bind <name> <service>|--global
outpost secret unbind <name> <service>|--global
outpost secret rm <name>         delete a secret
```

[AI models](#ai-models) too:

```
outpost ai                       the models in force, what Ollama has, and usage
outpost ai set llm|system1 <model> [--provider P]
                                 choose one, over config.toml's
outpost ai apps llm|system1 --all|--none|--reset|<app>...
                                 choose which apps may use it
outpost ai key <provider> [--value-stdin]
                                 save a hosted provider's API key, read hidden
outpost ai reset llm|system1     go back to config.toml's
outpost ai test llm|system1      send it one small request
outpost ai regenerate <service>  give an app a new token; it restarts
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
| `tab` | switch between Services, Users, Gateway and AI |
| `L` (Users, twice) | sign the selected user out everywhere |
| `enter` (AI) | choose a kind's model, or set a provider's key |
| `a`, `t`, `u` (AI) | choose which apps may use a kind, test it, go back to `config.toml`'s |
| `?` | expand the help |
| `q`, `esc`, `ctrl+c` | quit |

A service's detail pane also shows its catalog title and description, its
gateway address, and who may open it (`allow`, `hidden`, `public_paths`).
**Users** lists everyone, as `outpost user list` does, and where the selected
user is signed in. **Gateway** shows what `outpost gateway status` does, with
the gateway's recent activity. **AI** does what the portal's AI models page
does: it shows the LLM and System 1 model with their provider and apps, each
hosted provider and whether its key is set, and each app's usage. `enter` on a
kind opens a list of every provider's models to type into and narrow, which
also takes a model ID a provider doesn't list; `enter` on a provider asks for
its key, shown only as dots and never kept once it's sent; `a` chooses the
apps. When `config.toml` has changed since the daemon started, a line under
the title says the daemon needs a restart.

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
changes it at any time; run `outpost daemon restart` afterwards.

`[gateway]` turns on the login gateway; its settings are in
[The login gateway](#the-login-gateway). `[ai]` chooses the models apps are
given; see [AI models](#ai-models).

The daemon reads the file only when it starts. Manifests reload by themselves,
but a change here waits for a restart, so `outpost list`, `status`, `config`
and `gateway status` say when the file no longer matches what the daemon is
running. Comments and layout don't count as a change.

The file may hold an auth key, so every save makes it `0600`, including a file
first written by hand. `outpost config --authkey` writes one without
reinstalling; it reads the key from the terminal or stdin, never an argument,
and `outpost config` shows only that one is set. `outpost install --authkey`
saves it to the same file, never to the unit, which other users can read.
`TS_AUTHKEY` in the daemon's own environment wins over the file. Services and
tsnet nodes never inherit it, nor `GITHUB_TOKEN`.

## Secrets

API keys and other credentials don't have to sit in a manifest's `env`, where
anything that reads `~/.outpost/services/` sees them and changing one means
editing files. A secret is kept by the daemon instead and handed to services as
an environment variable:

```sh
outpost secret set openai-key --service tasking   # prompts for the value
outpost secret set sentry-dsn --global --value-stdin < dsn.txt
outpost secret list
```

A secret has a name, the variable it becomes (by default the name in upper
case, `openai-key` → `OPENAI_KEY`) and a value. It reaches a service through a
binding: to that service by name, or `--global` to every service. A secret bound
to a service by name wins over a global one for the same variable, so one app
can have its own key while the rest share one. Two secrets for one variable
can't be bound at the same level. A secret also wins over the manifest's own
`env` for the same variable, and the daemon logs that it did; `OUTPOST_`
variables, and `PATH`, `HOME` and the like, can't be secrets.

**Rotating restarts what uses it.** `outpost secret set` on a secret that
exists replaces its value. The supervisor notices on its next tick and restarts
every running service that gets it, since a process only reads its environment
once and the point of rotating is that the old value stops working. Binding,
unbinding and removing restart the services they affect the same way; a service
you stopped stays stopped and picks the change up when it next starts.

**From the portal.** With the [login gateway](#the-login-gateway) on, an admin
sets an app's own secrets under that app's **Manage** menu in the catalog: add
one for it, replace a value by pasting a new one, or take one away. Secrets
given to every app are on the **Secrets** page, linked from the catalog, which
also lists any no app gets. Each change goes in the gateway's activity feed
with who made it, never with a value.

**Values go in, never out.** Nothing reads a value back: not the page, not
`outpost secret list`, not the control socket. To check one, look at what the
service received. Values are typed at a prompt with no echo or piped in, never
put on the command line.

Secrets live in `secrets.db`, sealed with AES-256-GCM under a key in
`secrets.key` beside it, both mode `0600`. That keeps a copy of the database on
its own, such as a stray backup, from giving the values away. It isn't a wall
between local processes: anything running as you can read both files, and every
service runs as you (see [What Outpost trusts](#what-outpost-trusts)). Back the
two files up together; without the key, the database is unreadable, and the
daemon refuses to start rather than make a new key and lose them.

## AI models

Apps can use AI without each one choosing a model, pointing at a server and
holding a key. Outpost has one **LLM**, which generates text, and one
**System 1** model, which answers typed questions about an input with
probabilities in a single pass. Each comes from one provider:

| Provider | LLM | System 1 | Key |
|---|---|---|---|
| [Ollama](https://ollama.com), on this machine | Gemma 4, Qwen 3, … | Laya | none |
| OpenAI | GPT models | `gpt-6-luna`, through its Decisions API | yes |
| Anthropic | Claude models | — | yes |
| OpenRouter | its 380 or so chat models | its decision models: Jev, Liquid d1, … | yes |
| TypeSafe | — | Jev | yes |

Every app reaches them through Outpost, never the provider directly:

```
OUTPOST_AI_URL=http://127.0.0.1:47901
OUTPOST_AI_TOKEN=oai_…            # each app its own
```

An app calls the API it already speaks, with its SDK's base URL pointed at
Outpost and the token as the API key, and asks for `llm` or `system1`:

| API | Base URL | Model |
|---|---|---|
| OpenAI Chat Completions | `$OUTPOST_AI_URL/openai/v1` | `llm` |
| Anthropic Messages | `$OUTPOST_AI_URL/anthropic` | `llm` |
| System 1 | `POST $OUTPOST_AI_URL/systemone/v1/systemone` | `system1` |

Outpost replaces the `model` an app sends with the one chosen now, adds the
provider's key, and sends the request on, so changing the model, or the
provider, reaches every app on its next request with nothing restarted. Two
combinations need more than that:

- **OpenAI doesn't take the Anthropic Messages API.** While the LLM is an
  OpenAI model, an app calling `/anthropic` gets a 400 telling it to use
  `/openai/v1/chat/completions`; the page and `outpost ai` warn about this.
  Every other LLM provider takes both APIs.
- **OpenAI's decision model speaks its own Decisions API.** Apps still send
  System 1; Outpost translates the questions there and the answers back.

`outpost-service-spec.md` §15 has the app's side: the endpoints, worked
calls, the System 1 request and answer, and errors.

**Choosing them.** Either in `config.toml`:

```toml
[ai]
llm              = "claude-haiku-5-5"
llm_provider     = "anthropic"          # unset is ollama
system1          = "laya:latest"
# system1_provider = "ollama"
# llm_apps       = ["sous", "tasking"]  # unset is every app; [] is none
# system1_apps   = ["tasking"]
# ollama_url     = "http://127.0.0.1:11434"
# listen         = "127.0.0.1:47901"

# Only to move a provider, or keep its key under another secret's name:
# [ai.openai]
# base_url       = "https://my-proxy.example/openai"
# api_key_secret = "openai-api-key"
```

or, with the [login gateway](#the-login-gateway) on, on the admin-only **AI
models** page, which wins over the file, the same as an app's Internet access
setting does for Funnel. What each app may use, its token and what it has asked
for are under the app's **Manage** menu in the catalog. For each kind it offers:

- **Ollama's models** each time the page loads, sorted by what Ollama says
  each can do: `completion` for an LLM and `decision` for System 1, so Laya
  can't be picked as the LLM or Gemma as System 1;
- **each hosted provider's models,** from its own model list, kept for ten
  minutes, to search and pick from. OpenAI's decision model and TypeSafe's Jev
  have no list, so their usual names are suggested and any model ID can be
  typed in.

Saving checks the model is in the provider's list, when there is one; if the
list can't be had, nothing is saved. **Test** sends the model one small
request and shows the reply. "Use config.toml's" forgets the page's choice.

From the CLI:

```sh
outpost ai key anthropic                         # prompts for the key, hidden
outpost ai set llm claude-haiku-5-5 --provider anthropic
outpost ai set system1 typesafe/jev-1.14 --provider openrouter
outpost ai apps llm sous tasking                 # or --all, --none, --reset
outpost ai test llm
```

**Keys.** A hosted provider's key is entered on the page's Providers section
or with `outpost ai key <provider>`, and kept as a secret in `secrets.db`
(`openai-api-key` and so on), so it's sealed, shown on the Secrets page and
replaceable there. It isn't given to any app: Outpost adds it to each request
itself, so a key never sits in an app's environment or logs. Replacing it
takes effect on the next request.

**Which apps may use each.** Hosted models cost money per request, so each
kind can be given to every app, which is the default, or to the apps you
choose, on the page's **Apps…** or with `outpost ai apps`. An app left out
gets a 403 saying so, and isn't shown the model in `/openai/v1/models`.
Every app still gets the variables either way.

Outpost doesn't pull Ollama models. Run `ollama pull` yourself; if a chosen
model is later removed, or a provider stops listing it, the page and
`outpost ai` say so, the daemon logs it at start, and apps get a 503 naming
it until you choose another.

**What's counted.** Each request is one line in the daemon's log: the app,
the kind, the provider and model, the status, the time taken and the tokens.
Prompts and replies are never logged. The page and `outpost ai` show each
app's requests, failures and tokens since the daemon started. Changes go in
the gateway's activity feed, a key only as having changed.

**Tokens.** An app's token is derived from its name and a key in `ai.db`, so
it is the same every time the app starts, across daemon restarts too, and
nothing that can be read back is stored. **New token** on the page, or
`outpost ai regenerate <app>`, replaces it and restarts that app; the old one
stops working at once. The token is how Outpost tells apps apart, for
counting and for who may use what.

The endpoint listens on loopback only. It isn't on the tailnet, behind the
gateway or reachable through Funnel. The design is in
`docs/design/ai-models.md`.

## Mail

Apps can get and send email through one Gmail account, each at its own
**plus address**: mail to `you+yarr@gmail.com` is Yarr's, and so is mail to
`you+k7q2@gmail.com` when `k7q2` is a random alias made for Yarr. Outpost
checks the inbox every minute over IMAP, keeps each routed message for its
app, labels it `Outpost/<app>` in Gmail and archives it. Mail to no route is
never opened or moved.

Every app is started with:

```
OUTPOST_MAIL_URL=http://127.0.0.1:47902
OUTPOST_MAIL_TOKEN=omail_…        # each app its own
```

and reads its mail at `GET $OUTPOST_MAIL_URL/v1/messages`, and sends at
`POST /v1/send` if it's been allowed to. Each address also has an **Atom
feed**, so a feed reader subscribes to the newsletters sent to it: give a
newsletter `you+k7q2@gmail.com`, paste that alias's feed link into Yarr, and
each issue arrives as an article, with no change to Yarr.
`outpost-service-spec.md` §16 has the app's side.

**Setting it up.** Turn on 2-Step Verification for the Google account, make an
[app password](https://myaccount.google.com/apppasswords), and enter the
address and app password on the admin-only **Mail** page. The password is kept
as the secret `gmail-app-password` in `secrets.db`, never given to an app. Or,
without the portal:

```toml
[mail]
address = "you@gmail.com"
# password_secret = "gmail-app-password"   # set it with: outpost secret set gmail-app-password
# poll   = "1m"
# imap   = "imap.gmail.com:993"
# smtp   = "smtp.gmail.com:587"
# listen = "127.0.0.1:47902"
```

**Per app,** under its **Manage** menu in the catalog: its addresses and their
feed links, **New address…** to make an alias with a note saying what it's
for, **Remove…** to retire one, whether it may **send** (off by default; up to
100 messages a day, from its own address, so replies route back to it), and
its token. An app's inbox keeps its newest 500 messages.

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
# tailscale_name  = "outpost"            # the portal's own tailnet node; see below
```

With Tailscale on, that is all the setup it needs: each app is served at its
own node's tailnet address (below), with that node's certificate, and
`tailscale_name` puts the portal on the tailnet as a node of its own
([The portal on the tailnet](#the-portal-on-the-tailnet)). Without Tailscale, it needs a wildcard:
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
80 to this; `outpost daemon restart` picks it up.

### The portal on the tailnet

The gateway listens on loopback, so with nothing in front of it the portal
can't be reached from anywhere else, even when `public_url` names the
machine's tailnet address. `tailscale_name` gives the portal a tailnet node of
its own, run like a service's (`outpost __tsnet-node`, state in
`~/.outpost/tsnet/_portal/`), which serves HTTPS with the tailnet's
certificate and hands everything to the gateway:

```toml
[gateway]
enabled        = true
public_url     = "https://outpost.<tailnet>.ts.net"
tailscale_name = "outpost"
```

The node can't take the machine's own name, which the machine already has, so
it needs one of its own. It needs Tailscale on, and `public_url`'s host has to
be `<tailscale_name>.<something>`; Outpost refuses the config otherwise. No
service's `tailscale_name` may be the portal's. Like any node, it answers only
requests for its own name, so a peer can't name an app's host through it.

The portal's address is fixed when the daemon starts: its host, cookie names,
`OUTPOST_ISSUER` and the login links all come from `public_url`. So if
Tailscale gives the node another name because `outpost` is taken
(`outpost-1`), Outpost doesn't follow it. The daemon log says so in one line
naming both, and `outpost gateway status` shows a warning. Free the name in the
Tailscale admin console, or change `public_url` and `tailscale_name` to match,
and restart the daemon. `outpost gateway status` also shows the node's state
and, while it waits to be authorised, its login URL.

**Moving an existing setup over** is a one-time change by hand: set
`public_url = "https://outpost.<tailnet>.ts.net"` and
`tailscale_name = "outpost"` under `[gateway]`, then run
`outpost daemon restart`. Every service restarts with the daemon and picks up
the new `OUTPOST_ISSUER`. Browser sessions belong to the old host, so everyone
signs in again. If `tailscale serve` was forwarding the machine's name to the
gateway, it can be turned off with `tailscale serve reset`.

What the gateway decides goes in the activity feed, and `outpost gateway
activity` shows it: who signed in or out and from which address, failed and
throttled sign-ins, refused apps, password changes, use of the setup link,
access tokens created, revoked and rejected, and updates, starts, stops,
restarts and backups done from the portal.
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
lists where you're signed in (the browser and system, when and from which
address you signed in, roughly when you were last active, and the apps you
opened there), and signs out any one of those devices, or every one but the
one you're using, along with every app they opened. It also changes your
password, which signs out every other device, and signs you out everywhere at
once. `outpost user sessions <name>` shows an admin the same list.

An admin also gets a **Manage** menu on each app: Start, Restart, Back up now
and Stop, whichever make sense for the state it's in, and Update when a newer
release is waiting. Everything but Start asks first and says what it will do,
since each takes the app away for a while. They go through the daemon's API,
as `outpost start` and the rest do. The row says what's under way, the page
refreshes until it's done (holding off while a menu is open), and the outcome
stays on the row for five minutes.
`outpost gateway activity` records who did what.

An open catalog keeps up with apps restarting on their own, and catches up when
you come back to its tab after 15 seconds or more away. It doesn't refresh on a
timer otherwise, or at all while its tab is out of view, because each load renews
the session and a forgotten tab should still sign out. So a crash that happens
while you're watching a quiet page shows once you reload.

Each app also shows what it's using, to everyone who can see it: a line such
as `2.0%  84 MB  1.2 GB`, with an icon for CPU, memory and storage, and under
**Last 24 hours** a small graph of each:

- **CPU** is a percent of one core, as `top` shows it, so a busy app on
  several cores can pass 100%. Each point is the busiest half minute in its
  quarter hour.
- **Memory** is the resident memory of the app's whole process group.
- **Network** is the bytes the gateway carried to (in) and from (out) the
  app. Only traffic through Outpost's gateway is counted; anything that
  reaches the app directly, on loopback or its own tailnet node without the
  gateway, isn't.
- **Storage** is the size of the app's storage directory, measured every ten
  minutes, whether or not it runs. A walk that stops short says "at least".

A gap in a graph is time the app wasn't running. The history is kept in the
daemon's memory, a day of it, and starts again when the daemon restarts; until
there's a full day, the graphs fill in from the right and say when they start.
CPU and memory are read with `/bin/ps` on macOS and from `/proc` on Linux.

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
  group yet is logged, not refused, so a manifest can come first. It also
  decides who may open the app's [raw TCP forwards](#raw-tcp-forwards).
- **`public_paths`** are for webhooks and feeds that carry their own
  credentials. A pattern is an exact path or a prefix ending in `/*`. With
  [`funnel = true`](#funnel-webhooks-from-the-internet) the internet reaches
  them too. An app's Internet access setting, under its **Manage** menu in
  the portal, can set them instead, and then wins over the manifest.
  Requests there still have every `X-Outpost-*` header and Outpost's cookie
  removed, and only a path already in its simplest form matches, so
  `/webhook/../admin` gets no exemption.
- A release's `outpost.toml` may set `title`, `description` and
  `public_paths`, which `outpost add` copies into the manifest. Who gets in and
  where the app lives stay the operator's call.
- With the gateway on, a service name that can't be a DNS label is a manifest
  error unless `subdomain` or `hostname` gives it an address.

Not built yet: app icons. The catalog marks each app with its station.

### Access tokens

Nothing without a browser can sign in, so scripts, agents and MCP clients use a
personal access token instead:

```sh
TOKEN=$(outpost token create alice --name backup-cron --app yarr --expires 90d)
curl -H "Authorization: Bearer $TOKEN" https://yarr.outpost.example/api/items
```

Only the secret goes to stdout, and that is the only time it is shown; the
database keeps a SHA-256 of it. `outpost token list` shows each token's scope,
expiry and roughly when it was last used, and `outpost token revoke <id>` ends
one. Like the password a browser sends, a token needs TLS to be safe on a
network.

A request with a token is treated as that user: the same `allow` check on every
request, and the same `X-Outpost-*` headers and identity token, so an app can't
tell it from a browser. The app never sees the token itself.

- **An app with its own bearer auth.** Outpost only claims a bearer token that
  starts with `opat_`; any other `Authorization` reaches the app untouched. A
  client that needs both sends its app credential in `Authorization` and the
  Outpost token in `Outpost-Token`:

  ```sh
  curl -H "Authorization: Bearer $APP_KEY" -H "Outpost-Token: $TOKEN" \
    https://yarr.outpost.example/api/items
  ```

- **Scope.** A token reaches every app its user may open, or only those named
  with `--app` (repeatable, by service name). Naming an app grants nothing the
  `allow` list doesn't. `--expires` takes days (`90d`), weeks (`12w`) or hours
  (`24h`); without it a token doesn't expire.
- **Apps only.** A token is refused on the portal, apart from its
  [MCP endpoints](#mcp-clients), and under `/.outpost/` on app hosts, so it
  can't reach the account page, updates or sign-in, and it never creates a
  session or sets a cookie. Tokens are managed from the CLI.
- **A failed token is an answer, not a fallback.** A token that is wrong, expired,
  revoked or out of scope gets a plain `401` with `WWW-Authenticate: Bearer`, and
  one whose user the `allow` list refuses gets a plain `403`; neither is
  redirected to the login. That holds on `public_paths` too and with a good
  session cookie alongside: a request with a bad token isn't quietly treated as
  anonymous. Too many rejected tokens from one address get a `429` with
  `Retry-After`, good tokens included, until it backs off. With no token, public
  paths and browsers work as they always have. A good token on a public path
  gets what a signed-in browser does there: no identity headers.
- **Ending them.** Disabling or removing a user, `outpost user logout`, signing
  out everywhere from the account page, and changing or resetting a password all
  revoke the user's tokens, and enabling the user again doesn't bring them back.
  Ending one browser session on the account page leaves tokens alone.

#### MCP clients

A service that serves MCP over Streamable HTTP says where in its manifest:

```toml
mcp_path = "/mcp"
```

An MCP client with an access token can then reach it at one address on the
portal, `https://<portal>/mcp/<service>`, as well as at its `mcp_path` on the
service's own address, `https://<tailscale_name>.<tailnet>.ts.net`. With the
portal [on the tailnet](#the-portal-on-the-tailnet), from Claude Code:

```sh
export OUTPOST_TOKEN=$(outpost token create alice --name claude-code --app sous --expires 90d)
claude mcp add --transport http sous https://outpost.<tailnet>.ts.net/mcp/sous \
  --header 'Authorization: Bearer ${OUTPOST_TOKEN}'
```

Claude Desktop reaches it the same way through a local `mcp-remote` bridge.

A request to `/mcp/<service>` goes through everything any token request does:
the throttle, the token, its scope and the app's `allow` list. It then goes to
the service at its `mcp_path`, with the query string kept, with `Host` set to
the service's own host, and with the same identity headers and identity token
(`aud` the service's host) as anywhere else. It takes tokens only. With no
token it gets `401` with `WWW-Authenticate: Bearer`, never a sign-in page, and
a browser's session doesn't count. A bad, expired or out-of-scope token, a
service that doesn't exist and one with no `mcp_path` all get the same `401`,
so a token can't be used to find out which services there are. `outpost add
--mcp`, or a release whose descriptor says `mcp = true`, writes
`mcp_path = "/mcp"` for you; since a 401 can't say why, `outpost apply` and
the daemon's log warn about a manifest with `mcp = true` and no `mcp_path`.
Only the exact
path is routed: `/mcp/<service>/anything` isn't, and the old SSE transport,
which needs a second path, isn't supported. Responses stream, any `Set-Cookie`
the service sends is dropped (the portal's origin isn't the service's to set
cookies on), `Mcp-Session-Id` passes through, and a service that is down gets
a plain-text `502`. The portal answers OAuth discovery
(`/.well-known/oauth-protected-resource` and the like) with a plain `404`, so
a client whose token has stopped working reports that rather than looking for
a sign-in. There is no OAuth server, so connectors that run in Anthropic's
cloud, rather than on a machine on your tailnet, can't use this.

## What Outpost trusts

Outpost is built for one person's machine, and its trust boundaries follow
from that. Knowing where they are tells you what adding a service means.

**A service is trusted as much as Outpost itself.** Every service runs as the
same user as the daemon, with no sandbox. It can read everything under
`~/.outpost` — `auth.db` with its password hashes and the keys that sign
identity tokens, `secrets.db` and the key that unseals it, so every other
service's secrets and the AI providers' API keys, `ai.db`, from which any
app's AI token can be made,
`config.toml` with the Tailscale auth key, each node's
tailnet identity — write a manifest, which is running any command as that
user, and talk to the control socket, which can add an admin or mint an
access token. `outpost add <url>` installs a release binary from a GitHub
repository; that is a decision to trust its author with the whole box. Read
the repository before you add it.

**Services trust the gateway by its headers, and so does anything else on the
machine.** A service listens on loopback and reads `X-Outpost-User` from the
gateway. Nothing stops another process on the machine — another service,
say — connecting to that port directly and sending the same header with any
name in it. The identity token in `X-Outpost-Identity` is the way a service
tells the gateway from a neighbour; the [service contract](outpost-service-spec.md)
says when to verify it. The login front door keeps the outside out; it does
not keep services apart from each other.

**A public path behind Funnel is on the internet.** With `funnel = true`,
anyone can send it anything, as often as they like. Outpost makes sure only
those paths are reachable, anonymously, but it can't tell a real webhook
from a forged one: the app has to check the signature. See
[Funnel](#funnel-webhooks-from-the-internet).

**Loopback is a trusted proxy.** By default `trusted_proxies` is the loopback
range, because that is where the tailnet nodes and `tailscale serve` connect
from, and they say in `X-Forwarded-For` who the client was. So can any other
local process, which then chooses the address the activity feed records and
the sign-in throttle counts by. Set `trusted_proxies` to exactly what is in
front of the gateway if that matters to you.

**Apps are one site.** Without Tailscale, every app is at `<name>.<domain>`,
and browsers treat those as one site: a page in one app can make a request
to another with the user's cookie, and the gateway adds the user's identity
to it. The gateway checks the origin of its own forms, not of an app's. An
app behind the gateway needs its own check on requests that change things;
the service contract asks for one. On the tailnet each app has its own
`.ts.net` name, which browsers keep apart. Under plain http, with the
`outpost` cookie in place of `__Host-outpost`, apps can also set cookies for
each other; use https anywhere that matters.

**A tcp forward is only as narrow as the service behind it.** The gateway
decides which Outpost users' devices may open a [raw TCP
forward](#raw-tcp-forwards), but it can't tell the service who they are, and
anyone allowed in talks to the service directly. Treat the forwarded port as
you would one open to those users on the internet: the service's own
authentication is what stands between them and it.

**Tailscale sees its own logs.** Each node is a `tsnet` program, and by
default tsnet sends its logs to Tailscale's log service, as the Tailscale
client does. Nothing of Outpost's goes in them, but it is traffic you didn't
configure; `TS_NO_LOGS_NO_SUPPORT=true` in the daemon's environment turns it
off, at the cost of Tailscale's support.

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
  auth.db                  users, password hashes, sessions, signing keys,
                           linked tailnet logins (0600)
  secrets.db               secrets and their bindings, values sealed (0600)
  secrets.key              the key that seals them (0600)
  exposure.db              public paths and Funnel set on the portal, over
                           the manifests' (0600)
  ai.db                    AI models and their apps chosen on the portal,
                           and the key apps' AI tokens come from (0600)
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
storage, not this file. The same goes for `secrets.db`, which must be kept
with `secrets.key`, and for `exposure.db`, which records what the portal
set over the manifests: without it, each app goes back to its manifest's
public paths and Funnel. Without `ai.db`, the models go back to
`config.toml`'s and every app gets a new AI token when it next starts.

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
outpost daemon restart     # restart the daemon through it
```

`install` writes the file and prints the commands that would enable it, rather
than running them — registering a background service should be a deliberate act.
Pass `--enable` to have Outpost run them for you.

Pass `--authkey` to save a Tailscale auth key to `config.toml`, as
`outpost config --authkey` does; that reads it from the terminal instead, out of
the process table and shell history. `TS_AUTHKEY` in the shell running
`install` isn't copied into the unit file, which other users can read.

If `config.toml` doesn't yet say whether to use Tailscale, `install` checks
whether Tailscale is running on the machine, says what it found, and asks
whether each service should get its own Tailscale identity. The answer is
written to `config.toml`, so it's asked once. `--tailscale` and
`--no-tailscale` answer without asking, and so does an auth key: `--authkey`
means on, unless the home already has a setting or
`--no-tailscale` is given. Otherwise, with no terminal, a new install gets
Tailscale off and an existing one is left as it is.

### Updating Outpost itself

```sh
outpost update --self --check   # what is installed, what is latest; changes nothing
outpost update --self           # install the latest release and restart the daemon on it
outpost update --self --to v0.2.0
```

`update --self` downloads a release of Outpost from GitHub, checks it against the
release's `SHA256SUMS`, and replaces the binary the daemon's launchd job or
systemd unit runs. Before anything stops, it runs the download to make sure it
is the release asked for and runs on this machine, and copies `auth.db` to
`~/.outpost/backups/auth-<old release>-<time>.db`, since a new daemon may
migrate it and nothing else can rebuild your users. Only the two newest of
those copies are kept, since each holds password hashes and keys as they were,
including ones you have since changed. Then it restarts the daemon
through the job, so **every service restarts with it**, just as with
`outpost daemon restart`.

If the new daemon doesn't come back as the new release within `--wait`, the old
binary is put back and the daemon restarted on that. `auth.db` isn't put back:
the copy is there if you need it. Otherwise the old binary is kept as
`<binary>.previous`.

It needs the job, since nothing else could start the new daemon, so install one
first. It refuses, changing nothing, when:

- this `outpost` is a development build with uncommitted changes, or from
  `go install`. There's no telling which release is newer than that; `--to` a
  tag, or `--force` for the latest. A clean `make build` between tags, such as
  `v0.2.0-30-g6c3ba82`, is the exception: it was made from v0.2.0, so any
  later release is an update for it, and it is refused only when the latest
  is no later than v0.2.0.
- the job runs a different binary from the `outpost` you ran. It names the one to
  run instead.
- the binary belongs to a package manager, such as Homebrew or Nix, or to another
  user, or its directory isn't writable. Update it the way it was installed.
- the release publishes no checksum for it. `--allow-unverified` installs it
  anyway.
- a service update is running, since the restart would end it with no rollback.
  Wait for it to finish.
- a manifest doesn't validate, since the new daemon wouldn't start and every
  service would stay down. Fix it first.
- another `update --self` is running, for this home or for another home whose
  daemon runs the same binary.

`--force` reinstalls the release that is already installed, or installs over a
development build; it doesn't override any of the above.

While it runs, `outpost daemon restart` refuses unless given `--force`, naming
the update of Outpost itself.

Going back works as it does for a service: an older release is only installed
when you name it with `--to`, and then with a warning.

#### From the portal

With the gateway on, the Updates page has a row for Outpost, and an admin can
install its latest release from there. It is `update --self` with no flags:
the same checks, download, `auth.db` copy, wait and rollback. Pressing the
button starts `outpost __self-update` as a process of its own, since the
daemon can't outlive its own restart. That process replaces the binary and
stops the daemon with SIGTERM, and launchd or systemd starts it again on the
new binary. **Every app restarts with it.** The page waits it out and reloads
once Outpost is back, with the result. The activity feed records who started
it and how it ended.

The row says why instead of offering the button when Outpost can't be updated
from there: a development build (other than a clean one made from a release older than the latest), a binary a package manager or another user
owns, a daemon that wasn't started by its launchd job or systemd unit (and so
wouldn't be started again), or a systemd system service, which has to be
updated on the host with `sudo outpost update --self`. While it runs, apps'
Update buttons are turned off and the catalog's Manage actions are refused,
and it won't start while one of those is under way. Its progress is kept in
`~/.outpost/self-update.json`. Its output goes to
`~/.outpost/logs/self-update.log` on macOS, and to
`journalctl --user -u outpost-self-update-<label>` under systemd.

### Upgrading

```sh
make install              # replace the binary
outpost install           # if needed; same flags as before, such as --system
outpost daemon restart    # put the daemon on the new binary
```

`outpost install` is only needed if the binary has moved or a new version has
added a question: it rewrites the service file and asks. If the job is already
loaded it says so rather than offering to load it again. launchd keeps the
definition it loaded, so if the file changed it prints the commands that apply
it, and those restart the daemon too.

### Restarting the daemon

`outpost daemon restart` restarts the daemon through its launchd job or systemd
unit, then waits until a new daemon answers on the socket and reports its
version. It can tell the new daemon from the old one, which may still be
answering or stopping its services, and if none comes up in `--wait` (three
minutes by default) it says which of those it was waiting for and where the log
is.

Services go down with the daemon and come back with the new one, so expect a
short outage for each. The old daemon stops them in order, each with SIGTERM and
SIGKILL after 10 seconds. The new one starts them again, apart from any you
stopped and any halted after a crash loop, which stay that way. If stopping takes
longer than the job's 60-second limit, the init system kills the daemon; on
launchd services still running then are reclaimed as orphans when the new daemon
starts, and on systemd they're killed with it.

It refuses, changing nothing, when:

- no job is installed for this home. A daemon you started by hand in a terminal
  is left running; stop and start it there. A job with the same label that runs a
  different `--home` isn't touched either.
- an update is running, since the restart would end it with no rollback. `--force`
  goes ahead.
- a manifest doesn't validate, since the new daemon wouldn't start and every
  service would stay down. `--force` goes ahead here too.

It looks for a per-user job and then a boot-time one, and `--system` skips the
first. A boot-time job needs root, so it runs its commands through `sudo` unless
you are root already. A job installed with `--label` takes the same `--label`.

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

On Linux the units are `outpost-backup.timer` and `outpost-backup.service`, so
`journalctl -u outpost-backup` shows the runs. v0.1.0 and earlier named them
`backup.timer` and `backup.service`; scheduling again with `--enable`, or
`--unschedule`, clears those old units, and only if Outpost wrote them.

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

Without an auth key, a node that needs a login gets a one-time URL, once each
time the node starts; a daemon restart gives a new one. `outpost status
<service>` shows it, and the daemon log has it too. A node gets the name it
asks for only if no other device on the tailnet has it, otherwise Tailscale
appends `-1`. To take a name over from an old device, delete that device in
the Tailscale admin console *before* logging the new node in.
Each service costs one device on your tailnet, and so does the portal with
`tailscale_name` set; check your plan's limit before this scales past a
handful. `funnel = true` puts an app's public paths on the internet; see
[Funnel](#funnel-webhooks-from-the-internet).

The `TAILSCALE` column of `outpost list` is what the node is doing, not only
what the manifest asks for:

| Shown | Meaning |
|---|---|
| the node's name | on the tailnet, under the name it was given (`yarr-1` if `yarr` was taken) |
| `needs login` | waiting to be authorised; `outpost status` has the URL |
| `starting` | coming up |
| `retrying` | the last attempt failed; `outpost status` says why |
| `-` | no node: the service isn't running, or Tailscale is off |

Joining the tailnet happens in the background: `Attach` records intent and
returns, so a node that is slow to register — or waiting on a first-run login
URL — never stalls supervision or the control socket. A failed attach is retried
with backoff (2s, doubling to 60s) until it succeeds or the service is detached,
and the reconcile loop re-asserts membership for every running service, so a
service cannot end up silently off the tailnet.

### Raw TCP forwards

A node proxies HTTP. For anything else — SSH into a git server, say — a
manifest can ask the node to carry raw TCP as well:

```toml
port = 23232                       # HTTP, as ever
tcp  = { 22 = 23231, 9418 = 9418 } # tailnet port = port on 127.0.0.1
```

`ssh soft.<tailnet>.ts.net` then reaches whatever listens on
`127.0.0.1:23231`. The service binds loopback for these ports too.

The gateway's login can't stand in front of a byte stream, so **with the
gateway on**, the node asks it about every connection instead. Tailscale
says which login the calling device is signed in to; the gateway turns that
into an Outpost user and applies the same rules an app request meets after
sign-in: the user must exist and be enabled, and be on the app's `allow`
list if it has one. Anything else — an unknown login, a device that belongs
to tags rather than a person, a gateway that doesn't answer — and the
connection is closed before the service sees a byte. Each connection, let in
or not, is a line in the daemon log; refusals also reach the activity feed.

A login belongs to a user when it is linked to them, or, failing that, when
it is the email of the one user with no link of their own:

```sh
outpost user tailnet scott scott@github   # a login that isn't an email
outpost user tailnet scott off            # back to matching by email
```

The service gets the connection as it came, from `127.0.0.1`, with no idea
who is calling: there is no header to put an identity in. It still has to
authenticate the caller itself — SSH keys, for a git server. The gate decides
who may reach it at all. A connection that is open when its user is disabled
stays open until it ends.

**With the gateway off**, as for HTTP, the tailnet is the boundary: any
device on it can connect.

Forwards are validated with the rest of the manifest: each port once, no
tailnet port 80 or 443, which the node uses, and never a forward to the
service's own HTTP `port`, which would reach the app without passing the
gateway. Changing them replaces the node without restarting the service.

### Funnel: webhooks from the internet

Some apps need to hear from the public internet: GitHub, Stripe or a chat
platform calling a webhook. With the gateway on, an app can have **exactly its
`public_paths`** answered there, through Tailscale Funnel, at the address it
already has on the tailnet:

```toml
tailscale_name = "tasking"
public_paths   = ["/webhooks/github", "/hooks/*"]
funnel         = true
```

```
https://tasking.<tailnet>.ts.net/webhooks/github   from anywhere
https://tasking.<tailnet>.ts.net/                  tailnet only, behind the login, as before
```

Or set the same on the portal: an admin's catalog links to **Internet
access**, which lists each app's public paths and whether Funnel is on, with
the addresses the internet can use. **A setting made there wins over the
manifest**, which stays the default for any app the page hasn't set. The page
says when it is overriding the manifest, and what the manifest says, and
"Use the manifest's setting" goes back to it. Changing either restarts the
app's tailnet node, not the app.

Nothing else is reachable from the internet: not the app's other paths, not
other apps, not the portal, sign-in, `/mcp/<name>` or `/.outpost/`. That
holds because of where requests go, not because of a check on each one.
Funnel traffic arrives on a listener of its own on the app's node, which
sends it to a gateway listener of its own on loopback, and that listener has
code only for public paths:

- every `X-Outpost-*` header and Outpost's cookies are removed, and the app
  sees an anonymous request, as on a public path today;
- an Outpost access token (`opat_…` or `Outpost-Token`) is refused with a
  401, never passed on, so a leaked one can't be used from outside;
- the host must be the node's own name, for an app with Funnel on, and the
  path one of its public paths, with the same hardening against `..`, `%2f`
  and `;` tricks. Anything else is a bare 404, never a login page;
- bodies are capped at 25 MiB, GitHub's largest webhook; upgrades are
  refused; requests time out after a minute;
- an address that keeps being refused, a scanner say, is slowed down and
  then told to wait (429);
- each request is a line in the daemon log, with the client's address.

**The app still has to check each webhook's signature.** Only it has the
secret GitHub or Stripe signs with. Outpost's part is that only the paths the
app chose are reachable, with the body intact. Treat a public path behind
Funnel as being on the internet, because it is: its hostname is in public
certificate logs, and scanners will find it within minutes.

Funnel needs, in Tailscale:

- HTTPS turned on for the tailnet, which the nodes already need;
- the `funnel` attribute for the app's node in the tailnet policy. If yours
  doesn't grant it, add:

  ```json
  "nodeAttrs": [{ "target": ["autogroup:member"], "attr": ["funnel"] }]
  ```

  or narrow `target` to your own user.

Until then, the node serves the tailnet as usual and `outpost status <name>`
and the app's Internet access setting in the portal say Funnel is unavailable, with Tailscale's
reason. The node keeps trying, so granting the attribute is enough; no
restart is needed. Admins see a **Public** mark on an app in the catalog
while the internet can reach it.

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
`backup --schedule`, `--tailnet`, `secret`, `ai`. `restore` takes the timestamp as optional,
defaulting to the newest archive. Not built: `outpost hermes publish`, which
spec §12 lists.

**A service may expose more than HTTP.** Spec §2 has a service listen on one
TCP port, and that port is HTTP. The manifest's `tcp` table adds raw TCP
forwards on the service's node, for protocols such as SSH, gated by tailnet
identity when the gateway is on; see [Raw TCP forwards](#raw-tcp-forwards).
The HTTP port is still the one Outpost probes and the gateway fronts.

**Outpost keeps secrets.** The spec has none; a credential went in a
manifest's `env`. `secrets.db` now holds values the daemon hands services as
environment variables, bound per service or to every one, and restarts what
uses one when it changes; see [Secrets](#secrets). The manifest is still the
source of truth for everything else: a binding to a service that isn't
declared does nothing until it is.

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

**`funnel` is built.** Spec §1 has Funnel out of scope and §7 has
`funnel = true` refused until it was built deliberately. It now answers an
app's `public_paths`, and nothing else, from the internet; see
[Funnel](#funnel-webhooks-from-the-internet) and the design in
`docs/design/funnel-webhooks.md`. `/mcp/<name>` is not exposed that way.

**The portal can override a manifest.** For `funnel` and `public_paths`
only, what an admin sets under an app's Manage menu in the portal is kept in
`exposure.db` and wins over the manifest, which stays the default. Like `auth.db` and
`secrets.db`, that file is authoritative rather than a cache.

**Outpost gives apps AI models.** The spec has nothing like it. `ai.db`
holds the models chosen on the portal and the key app tokens are derived
from; like `exposure.db`, it is authoritative rather than a cache. See
[AI models](#ai-models).

**Backup scheduling uses the init system.** Spec §10 leaves this open; Outpost
generates a launchd job or a systemd timer rather than growing a scheduler.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md): branch from `main`, run `make check`,
and keep each PR to one logical change. Questions go in an issue, see
[SUPPORT.md](SUPPORT.md); security problems go through
[SECURITY.md](SECURITY.md). Everyone taking part is expected to follow the
[code of conduct](CODE_OF_CONDUCT.md).

## License

[MIT](LICENSE).

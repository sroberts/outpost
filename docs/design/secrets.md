# Secrets

Built in PR #116, released in v0.3.0. The operator-facing description is in
the README under [Secrets](../../README.md#secrets), and the service's side is
in [`outpost-service-spec.md`](../../outpost-service-spec.md) §3. This note
records why it is shaped the way it is.

## The problem

A service's credentials, such as API keys, webhook signing secrets and DSNs,
had nowhere to go but the manifest's `env`. That has three costs:

- **Exposure.** The manifests are plaintext TOML that anything reading
  `~/.outpost/services/` sees, and they end up in places like dotfile repos
  and screenshots.
- **Rotation.** Changing a key meant editing a file on the host, then running
  `outpost apply`.
- **Sharing.** A key used by several apps had to be copied into each manifest,
  and rotating it meant finding every copy.

The goal was to rotate a key from the portal in a few seconds, and to bind a
secret either to particular services or to all of them.

## What was built

- **`internal/secrets`.** A store in `secrets.db` (SQLite) with three tables:
  - `secrets`: name, variable, sealed value and a revision counter;
  - `bindings`: `(secret, service)` pairs, where `service = ''` means every
    service;
  - `meta`.

  Values are sealed with AES-256-GCM under a 32-byte key in `secrets.key`.
- **The supervisor.** It asks the store for a service's variables when it
  starts the service, and checks a revision on every tick to restart the
  service when they change.
- **IPC commands and the CLI.** `secret.list`, `set`, `rm`, `bind` and `unbind`
  over the control socket, and `outpost secret …` to use them.
- **The portal.** An admin-only Secrets page in the gateway, with add, rotate,
  choose apps and remove.

## Decisions

**A store of its own, apart from `state.db`.** `state.db` is a cache of
observed reality that can be deleted and rebuilt from the manifests, and
nothing authoritative may live there. Secrets can't be rebuilt from anything,
the same as `auth.db`'s users. They aren't in `auth.db` either, because the two
have different lifecycles: `update --self` snapshots `auth.db` before a new
daemon can migrate it, and putting service credentials into those snapshots
would multiply the copies of every key.

**Not in the manifest, even by reference.** The manifest is the source of
truth for *what runs*. A binding lives in the store instead of in a
`secrets = ["openai-key"]` field. Two things argue for that:

- Making a secret global, or moving it from one app to another, then changes
  nothing on disk under `services/`, so it can be done from the portal, which
  never writes manifests.
- A manifest still reads the same on another host that has no such secret.

The cost is that `cat`ting a manifest no longer shows everything a service
gets. `outpost secret list` and the portal page show that instead.

**Sealed at rest, and honest about what that buys.** Encrypting
`secrets.db`'s values means a copy of the database on its own, such as a
stray backup or a file synced somewhere, doesn't give them away. It is not a
barrier between local processes. The key sits beside the database, mode
`0600`, and every service runs as the same user as the daemon, so anything
that can read one file can read the other. The README says so in "What
Outpost trusts" instead of letting the encryption imply more.

Each value is sealed with the secret's name as additional data, so a sealed
blob moved to another row fails to open, and a value can't quietly reach a
different service. The database file is created `0600` before SQLite opens it,
so its WAL and SHM files inherit that mode, as `auth.db`'s do.

**A missing key is fatal, not repaired.** The daemon makes a key on first
start, written with `O_EXCL` so two daemons racing can't each write their
own. Once a single secret is sealed, a missing or wrong-sized key stops
startup with a message about restoring it. Quietly making a new key would
leave every stored value unreadable for good, and a daemon that refuses to
start is a clear, recoverable error. Losing secrets isn't.

**Global and by-name bindings, and a fixed precedence.** These are the cases
that came up in practice:

- one key shared by every app, such as a Sentry DSN;
- a key for one app only;
- a shared key with one app overriding it.

So a secret is bound either to every service or to named services, and one
bound to a service by name wins over a global one for the same variable. The
order a service's environment is put together in, last wins:

1. the manifest's `env`;
2. global secrets;
3. secrets bound to the service by name;
4. `OUTPOST_*`, which Outpost sets itself.

**Clashes are refused, not resolved.** Binding a second secret for the same
variable at the same level, two global or two for the same service, is
refused, naming the secret that already holds the variable. Picking one by
name or creation time would make which value an app sees an accident of
ordering. A refusal is easy to act on. `Bind` checks for a clash and inserts
in one transaction, over a single database connection, so two changes at once
can't both pass the check.

**A secret beats the manifest's `env`, and says so.** When a manifest still
sets a variable that a secret now provides, the secret wins, and the daemon
logs `secret replaces env X from the manifest`. The other order would make
moving a credential out of a manifest a two-step change that silently does
nothing until the second step. The log line is what stops it being a surprise.
This was flagged on the PR as a decision for the repository owner.

**Some variables can't be secrets.** Names starting with `OUTPOST_` are
Outpost's contract with the service, and `PATH`, `HOME`, `USER`, `LOGNAME`,
`SHELL`, `PWD` and `TMPDIR` decide how the process starts and where it looks
for things. A secret is meant to hand over a credential, not to change how a
service is run.

**Restart on change, through the reconcile loop.** A process reads its
environment once, so a new value only takes effect when the process restarts.
The point of rotating a key is for the old one to stop working, so the
supervisor restarts every *running* service a change reaches. A service the
operator stopped stays stopped and gets the change when it next starts.

The change is detected rather than pushed. `Resolve(service)` returns the
variables along with a revision: a hash of `variable=secret@revision` entries
for what the service gets, truncated to 16 hex characters. The supervisor
stores the revision a service was started with and compares it on each tick.

This keeps the single-threaded reconcile loop the only thing that starts and
stops services. An IPC call or a portal form changes the store and returns,
and the loop does the rest on its next tick, the same path a manifest edit
takes. The revision is made from names and counters, never values, so it can
be logged and kept in memory next to service state without becoming a second
copy of a secret.

**An in-memory cache.** The store keeps every secret decrypted in memory and
reloads from the database after each write. The loop calls `Resolve` for every
service on every tick, and a disk read and an AES-GCM open per tick per
service would be pointless work. The daemon's memory already holds the values
it hands to the processes it starts, so the cache adds nothing an attacker
with access to the process couldn't already get.

**Values go in, never out.** Nothing returns a value: not `List`, not `Get`,
not the IPC protocol, not the page. `secrets.Secret`, the type everything
outside the package sees, has no value field, so a new caller can't leak one
by accident. Values are entered without echo at a prompt, or piped in with
`--value-stdin`, and are never accepted as command-line arguments, where `ps`
and shell history would show them. To check what a service got, look at what
the service received.

For the same reason, every change is recorded in the gateway's activity feed
and the daemon log with who made it and the secret's name, never its value.
The tests check the logs, the database file and its WAL for the value.

**The portal, as an admin page like Updates.** The page uses the gateway's
existing admin pattern:

- `adminUser` limits it to admins;
- `sameOrigin` checks every POST;
- `MaxBytesReader` caps the form, sized to the largest value form-encoded;
- every change goes through post/redirect/get.

Value fields are `type=password` with `autocomplete=off`. A failed form is
shown again with the name and variable as typed, but never the value. The
confirmation after a change appears on that secret's row, where the redirect
lands, and names the apps restarting. The page explains a global secret that
an app's own secret overrides ("Sous has its own API_KEY") rather than leaving
that app missing from the "in use by" list unexplained.

## Not done

- **No secrets as files.** Everything arrives as an environment variable. A
  service that wants a credential file, such as a TLS key or a service-account
  JSON, has to write it into its storage directory itself. The spec's §10
  table says so.
- **Not in `outpost backup`.** `backup` covers service storage. `secrets.db`
  and `secrets.key` have to be backed up together, by hand, like `auth.db`.
- **No key rotation.** `secrets.key` can't be replaced. That would mean
  re-sealing every value under a new key in one transaction, and since the
  encryption only protects copies of the database, there was no urgent need.
- **No restart batching.** Saving which apps get a secret is several store
  writes. If a tick lands between them, an affected app can restart twice
  for one save. That is harmless, but noticeable.
- **No way to opt out of restarts.** Every change restarts the apps it
  reaches. A service that could reload a credential without restarting has no
  way to say so. This was flagged on the PR as a decision for the repository
  owner, alongside the precedence over `env`.
- **No per-secret access control.** Any admin can manage every secret, and
  non-admins can't see the page at all. Outpost has one admin role, and this
  follows it.

## Since: provider keys for AI models

The AI models feature (`docs/design/ai-models.md`) keeps hosted providers'
API keys as secrets bound to no service. To send one, the daemon reads it
with `Store.Value`, added for that and called nowhere else: not by the
control socket, the portal or the CLI, so "values go in, never out" still
holds for everything outside the daemon.

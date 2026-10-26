# Plan: PR 1, the auth store and the user CLI

This is the build plan for the first delivery step in
[`gateway-users-catalog.md`](gateway-users-catalog.md). It has no HTTP. It
lands the store that the gateway (PR 2) builds on, and the commands an operator
uses to manage people.

Branch from `main` as `feat/auth-store`.

## Scope change from the design

The design put Tailscale detection and the new install-time default in this
PR. They're a separate logical change (CONTRIBUTING: one per PR), and the only
part that touches existing tests' assumptions, so they get their own PR,
**PR 1b**, outlined at the end. Nothing else here depends on them.

## What ships

- `internal/auth`: users, groups, sessions and signing keys in
  `~/.outpost/auth.db`, plus password hashing.
- IPC commands for user management, served by the daemon.
- The CLI commands `outpost user …` and `outpost auth rotate-key`.
- `outpost serve` opens `auth.db` and hands it to the IPC server.

Not in this PR: login, cookies, one-time codes, the JWT itself, any HTTP. The
first-run setup URL needs the gateway and moves to PR 2;
`outpost user add --admin` bootstraps until then.

## Files

```
internal/auth/
  doc.go            package comment: what lives here, why auth.db is not state.db
  names.go          ValidUsername, ValidGroup, MinPasswordLen
  password.go       argon2id hash/verify/needs-rehash (no DB)
  token.go          NewToken, HashToken (no DB)
  keys.go           Ed25519 key generation, JWKS rendering (no DB)
  store.go          Open, schema, Close
  users.go          user and group CRUD
  sessions.go       session create/lookup/touch/revoke
  keystore.go       signing-key persistence and rotation
  *_test.go         one per file above
internal/ipc/
  protocol.go       new Cmd constants and Request/Response fields
  server.go         UseAuth setter, dispatch cases
  client.go         typed client methods
  auth_test.go      new; existing ipc tests untouched
cmd/outpost/
  user_cmd.go       outpost user …, outpost auth rotate-key
  user_cmd_test.go  new
  serve.go          open auth.db, srv.UseAuth(store)
  main.go           dispatch cases, usage text, authPath()
```

`golang.org/x/crypto` (argon2) and `golang.org/x/term` (password prompt) are
already in `go.sum` as indirect dependencies. `go mod tidy` promotes them to
direct. Both are pure Go, so nothing changes for `CGO_ENABLED=0`.

## `internal/auth`

### Names (`names.go`)

```go
// Usernames are what apps see in X-Outpost-User, so they must be safe as a
// header value, a path segment and a database key without escaping.
var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

func ValidUsername(s string) error // nil or an error naming the rule
func ValidGroup(s string) error    // same rule
const MinPasswordLen = 12
func ValidPassword(p string) error // length only; no composition rules
```

No reserved group names: groups are only labels, and admin is a flag on the
user, not a group.

### Passwords (`password.go`)

PHC string format, so the parameters travel with the hash:
`$argon2id$v=19$m=65536,t=3,p=1$<salt b64>$<key b64>` (raw std base64, no
padding).

```go
type Params struct{ Memory uint32; Time uint32; Threads uint8; SaltLen, KeyLen uint32 }
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 1, SaltLen: 16, KeyLen: 32}

func HashPassword(pw string, p Params) (string, error)
func VerifyPassword(pw, encoded string) (ok bool, err error) // subtle.ConstantTimeCompare
func NeedsRehash(encoded string, p Params) bool
```

`VerifyPassword` returns an error only for a malformed hash, never for a wrong
password. It caps the parameters it will honour (memory at 1 GiB, time at 10),
so a tampered row can't make a login exhaust memory.

For tests, `testParams` (memory 64 KiB, time 1) keeps the suite fast. The
store takes `Params` as a field, defaulting to `DefaultParams`, and tests set
the fast one.

### Tokens (`token.go`)

```go
func NewToken() (string, error)  // 32 random bytes, base64url, no padding
func HashToken(tok string) string // hex(sha256(tok)); what the DB stores
```

### Keys (`keys.go`)

```go
type SigningKey struct {
    ID        string // kid: base64url(sha256(pub))[:16]
    Private   ed25519.PrivateKey
    CreatedAt time.Time
    RetiredAt time.Time // zero while current
}
func GenerateKey(now time.Time) (SigningKey, error)
func JWKS(keys []SigningKey) ([]byte, error) // {"keys":[{"kty":"OKP","crv":"Ed25519","x":…,"kid":…,"alg":"EdDSA","use":"sig"}]}
```

PR 2 adds signing. PR 1 only proves keys round-trip and render.

### Store (`store.go`)

Same DSN shape as `internal/state`: WAL, `busy_timeout(5000)`,
`foreign_keys(on)`. The file is created 0600; chmod after open, because
SQLite creates it with the process umask.

```sql
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    email         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',   -- empty: no password set, cannot log in
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS user_groups (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    PRIMARY KEY (user_id, name)
);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_hash  TEXT NOT NULL DEFAULT '',   -- app sessions point at their portal session
    app          TEXT NOT NULL DEFAULT '',   -- '' for the portal session
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_hash);
CREATE TABLE IF NOT EXISTS signing_keys (
    kid        TEXT PRIMARY KEY,
    private    BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    retired_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
-- meta: schema_version = 1
```

Times are unix seconds, matching `internal/state`.

```go
type Store struct { db *sql.DB; Params Params; now func() time.Time }
func Open(path string) (*Store, error)
func (s *Store) Close() error
```

`now` is injectable so session-expiry tests don't sleep.

### Users and groups (`users.go`)

```go
type User struct {
    Username, DisplayName, Email string
    Admin, Disabled              bool
    Groups                       []string // sorted
    HasPassword                  bool
    CreatedAt, LastLoginAt       time.Time
    Sessions                     int // live sessions, for `user list`
}

var ErrUserExists, ErrNoSuchUser, ErrBadCredentials = errors.New(…)

func (s *Store) AddUser(u User, password string) error // password may be empty
func (s *Store) SetPassword(username, password string) error   // + revokes all sessions
func (s *Store) RemoveUser(username string) error             // cascade removes sessions
func (s *Store) SetDisabled(username string, disabled bool) error // disabling revokes sessions
func (s *Store) SetAdmin(username string, admin bool) error
func (s *Store) UpdateGroups(username string, add, remove []string) ([]string, error)
func (s *Store) GetUser(username string) (User, error)
func (s *Store) ListUsers() ([]User, error)
func (s *Store) CountAdmins() (int, error)

// Authenticate is for the gateway (PR 2), but lives here so its timing rules
// are tested next to the hash code: an unknown user verifies against a dummy
// hash, so both failures take the same time and return ErrBadCredentials.
func (s *Store) Authenticate(username, password string) (User, error)
```

Two guards against locking yourself out: `RemoveUser`, `SetDisabled(true)` and
`SetAdmin(false)` refuse to act on the last enabled admin, with an error that
says so.

### Sessions (`sessions.go`)

```go
const PortalSessionTTL = 30 * 24 * time.Hour
const RefreshAfter     = 24 * time.Hour

type Session struct { Username, App string; ExpiresAt, CreatedAt time.Time }

func (s *Store) CreateSession(username, app, parentToken, userAgent string) (token string, err error)
func (s *Store) LookupSession(token string) (Session, User, error) // expired or disabled: ErrNoSuchSession
func (s *Store) TouchSession(token string) error                   // slides expiry when older than RefreshAfter
func (s *Store) RevokeSession(token string) error                  // also revokes children
func (s *Store) RevokeUserSessions(username string) (int, error)
func (s *Store) PruneExpired() (int, error)
```

PR 1 exercises these only from tests and `outpost user logout`. Nothing
creates a real session until PR 2.

### Signing keys (`keystore.go`)

```go
func (s *Store) CurrentKey() (SigningKey, error)    // generates the first on demand
func (s *Store) PublishedKeys() ([]SigningKey, error) // current + retired within 24h
func (s *Store) RotateKey() (SigningKey, error)     // retires current, makes a new one, prunes old
```

## IPC

Additions to `protocol.go`:

```go
CmdUserAdd     = "user.add"
CmdUserPasswd  = "user.passwd"
CmdUserRemove  = "user.rm"
CmdUserList    = "user.list"
CmdUserGroups  = "user.groups"
CmdUserDisable = "user.disable"
CmdUserEnable  = "user.enable"
CmdUserLogout  = "user.logout"
CmdAuthRotate  = "auth.rotate-key"
```

`Request` gains `User *UserRequest` (`omitempty`), carrying username, display
name, email, admin, password, add groups and remove groups. A pointer keeps
existing requests byte-identical on the wire. `Response` gains
`Users []UserInfo` (`omitempty`), a JSON-tagged copy of `auth.User` with no
hash fields.

`server.go`:

- **Don't change `Listen`'s signature.** Existing tests call it. Add
  `func (s *Server) UseAuth(a *auth.Store)`, called before `Serve`.
- `dispatch`: each `user.*` and `auth.*` case returns `"user management is
  not available: this daemon was started without an auth store"` when `auth`
  is nil, so the error is clear rather than a nil-pointer panic.
- Never log a request body. Requests now carry passwords.

`client.go`: one method per command, following the existing shape
(`func (c *Client) UserAdd(r UserRequest) (string, error)` and so on).

## CLI (`cmd/outpost/user_cmd.go`)

```
outpost user add <name> [--admin] [--email E] [--display-name N] [--group G]... [--no-password] [--password-stdin]
outpost user passwd <name> [--password-stdin]
outpost user rm <name>
outpost user list
outpost user groups <name> +family -kids
outpost user disable <name> | enable <name>
outpost user logout <name>
outpost auth rotate-key
```

- **Password input:** when stdin is a TTY, prompt twice with
  `term.ReadPassword`, and check the two match and the length rule *before*
  calling the daemon. `--password-stdin` reads one line for scripts. No
  password flag, because a password would end up in shell history and the
  process table.
- **`user add` with neither option** prompts. `--no-password` creates a user
  who can't log in until `passwd`; that's useful for adding someone's account
  before handing it over.
- **`user list`** is a tabwriter table: NAME, ADMIN, GROUPS, STATUS
  (`active`, `disabled`, `no password`), LAST LOGIN, SESSIONS.
- **No daemon:** the same `errNoDaemon()` as other commands. The CLI never
  opens `auth.db` itself (CLAUDE.md: one core, multiple interfaces).
- Add the commands to the `usage` string under a "Users" heading.

## `serve.go`

After `state.Open`:

```go
watch.at("opening the auth store")
authStore, err := auth.Open(a.authPath())  // ~/.outpost/auth.db
if err != nil { return err }
defer authStore.Close()
```

After `ipc.Listen`: `srv.UseAuth(authStore)`. If there are no users, log
`outpost: no users yet; create the first admin with "outpost user add <name> --admin"`.

## Tests

Every test is new. **No existing test may change.** If one fails, the change
is wrong, not the test.

- **`names_test.go`:** table of valid and invalid usernames, groups and
  passwords. Include the boundary lengths, uppercase, leading dot, and
  non-ASCII.
- **`password_test.go`:**
  - hash then verify;
  - a wrong password is false with a nil error;
  - a malformed hash returns an error;
  - `NeedsRehash` trips on changed parameters;
  - absurd parameters are refused;
  - the same password hashes to two different strings (salt).
- **`token_test.go`:** length and alphabet; `HashToken` is deterministic and
  never equals its input.
- **`keys_test.go`:** generate a key, then check the JWKS parses, has the
  right `kty`/`crv`/`alg`, and its `x` verifies a signature made with the
  private key.
- **`store_test.go`** (temp dir):
  - `Open` creates the file with mode 0600;
  - a second `Open` on the same file is fine;
  - the schema version is recorded.
- **`users_test.go`:**
  - add, get and list, with groups returned sorted;
  - duplicate add is `ErrUserExists`;
  - `passwd` revokes sessions;
  - `rm` cascades to sessions;
  - disable revokes sessions and blocks `Authenticate`;
  - the last-admin guards hold for all three operations;
  - `Authenticate` returns `ErrBadCredentials` for both an unknown user and a
    wrong password;
  - `Authenticate` rehashes when parameters change.
- **`sessions_test.go`**, with an injected clock:
  - create and lookup;
  - expiry;
  - touch slides only after `RefreshAfter`;
  - revoking a parent revokes its children;
  - a disabled user's sessions don't look up;
  - `PruneExpired`.
- **`keystore_test.go`:**
  - the first `CurrentKey` creates a key;
  - rotation keeps the old key published for 24h, then prunes it.
- **`ipc/auth_test.go`:**
  - a server with `UseAuth`: each command round-trips through a real socket
    (use the short-directory helper; darwin's 104-byte limit);
  - without `UseAuth`, each command returns the clear error;
  - the password never appears in anything logged.
- **`cmd/outpost/user_cmd_test.go`:**
  - `--password-stdin` paths for add and passwd;
  - a mismatched or short password is rejected before the daemon is called;
  - `user list` table output;
  - no daemon gives the standard message.

## Verify on the build machine

```sh
git fetch && git checkout -b feat/auth-store origin/main
# … implement …
go mod tidy                      # promotes x/crypto and x/term to direct
make fmt                         # never gofmt -w a directory
make check                       # what CI runs: fmt-check, vet, golangci-lint, test, race
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /dev/null ./cmd/outpost   # still cgo-free
```

Manual check:

```sh
make build
./outpost --home /tmp/op serve &                       # logs "no users yet"
./outpost --home /tmp/op user add alice --admin        # prompts twice
printf 'correct horse battery\n' | ./outpost --home /tmp/op user add bob --password-stdin --group family
./outpost --home /tmp/op user list
./outpost --home /tmp/op user rm alice                 # refused: last admin
./outpost --home /tmp/op auth rotate-key
ls -l /tmp/op/auth.db                                  # -rw-------
sqlite3 /tmp/op/auth.db 'select username, password_hash from users'   # hashes only, $argon2id$…
```

## PR 1b: Tailscale off by default, offered when found

A separate branch, `feat/tailscale-default`, after PR 1 or alongside it:

- **`internal/tsdetect`:** `Detect(ctx) Result{Found bool; Tailnet string}`.
  It runs `tailscale status --json` (on `PATH`, or the macOS app path) with a
  2s timeout, and any failure means not found. Test it through an injectable
  command runner, the same pattern as the tsnet `startNode` seam.
- **`config`:** `outpost config --tailscale on|off` writes the explicit value.
  `Load`'s built-in default stays **on**, so
  `TestLoad_MissingFileUsesDefaults` and `TestLoad_PartialFileKeepsDefaults`
  stay unchanged.
- **`install`:** new `--tailscale` and `--no-tailscale` flags. With no
  explicit value in `config.toml` and a TTY, it reports what detection found
  and asks; the prompt defaults to yes if a `tsnet/` state directory already
  exists, otherwise no. With no TTY and neither flag, it writes `false` for a
  new home and leaves an existing one alone. `--print` writes nothing.
- **First run:** writing `enabled = false` when `serve` creates a fresh home.
  Check first whether any existing `cmd/outpost` test starts `serve` on a
  fresh temp home and then relies on Tailscale being required. If one does,
  leave this part out and ask, rather than editing the test.
- **Docs:** the README's upgrade step becomes "replace the binary, re-run
  `outpost install`".

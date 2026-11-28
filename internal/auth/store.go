package auth

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = "1"

const schema = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    email         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0,
    tailnet_login TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS user_groups (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    PRIMARY KEY (user_id, name)
);

CREATE TABLE IF NOT EXISTS sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_hash  TEXT NOT NULL DEFAULT '',
    app          TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS sessions_parent ON sessions(parent_hash);

CREATE TABLE IF NOT EXISTS signing_keys (
    kid        TEXT PRIMARY KEY,
    private    BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    retired_at INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS access_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused, so a stale id can't revoke someone else's
    token_hash   TEXT NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0,
    internet     INTEGER NOT NULL DEFAULT 0,
    -- When the token was first used from the internet, so the activity feed
    -- can say so once. Nothing sets it until the gateway accepts these tokens
    -- over Funnel; it's added with internet so auth.db changes once.
    internet_used_at INTEGER NOT NULL DEFAULT 0,
    -- The OAuth grant that issued it, or 0 for one made with
    -- outpost token create. A grant's tokens go when it does.
    grant_id INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS access_tokens_user ON access_tokens(user_id);

CREATE TABLE IF NOT EXISTS access_token_apps (
    token_id INTEGER NOT NULL REFERENCES access_tokens(id) ON DELETE CASCADE,
    app      TEXT NOT NULL,
    PRIMARY KEY (token_id, app)
);

-- An OAuth client's access to one app's MCP endpoint, as one user. Only the
-- current refresh token's hash and the one before it are kept: the one before
-- is how a replayed refresh token is recognised.
CREATE TABLE IF NOT EXISTS oauth_grants (
    id            INTEGER PRIMARY KEY AUTOINCREMENT, -- never reused, like access tokens
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app           TEXT NOT NULL,
    client_id     TEXT NOT NULL,
    client_name   TEXT NOT NULL DEFAULT '',
    refresh_hash  TEXT NOT NULL UNIQUE,
    previous_hash TEXT NOT NULL DEFAULT '',
    created_at    INTEGER NOT NULL,
    refreshed_at  INTEGER NOT NULL DEFAULT 0,
    expires_at    INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS oauth_grants_user ON oauth_grants(user_id);
CREATE INDEX IF NOT EXISTS oauth_grants_previous ON oauth_grants(previous_hash);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// Store is the SQLite-backed auth store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	// Params are the argon2id settings for new hashes. Tests lower them.
	Params Params
	// now is the clock, injectable so expiry tests don't sleep.
	now func() time.Time

	dummyOnce sync.Once
	dummy     string
}

// Open opens (creating if needed) the auth database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create auth dir: %w", err)
		}
	}
	// Create the file owner-only before SQLite sees it: SQLite creates it with
	// the process umask, and gives its -wal and -shm files the main file's
	// mode, so getting the main file right first covers all three. The chmod
	// catches a file that already existed with a looser mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create auth db: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod auth db: %w", err)
	}

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open auth db: %w", err)
	}
	// One connection serialises every read-check-write sequence (the
	// last-admin guards, key rotation) without relying on SQLite upgrading a
	// read lock mid-transaction, which busy_timeout can't wait out. Traffic
	// here is a handful of logins, not a hot path.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open auth db %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply auth schema: %w", err)
	}
	if err := addSessionIP(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := addTailnetLogin(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := addTokenInternet(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := addTokenGrant(db); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('schema_version', ?)`, schemaVersion); err != nil {
		db.Close()
		return nil, fmt.Errorf("record auth schema version: %w", err)
	}
	return &Store{db: db, Params: DefaultParams, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion reports the schema version recorded in the database.
func (s *Store) SchemaVersion() (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&v)
	return v, err
}

// rollback undoes tx if it wasn't committed. The error is dropped on purpose:
// after a Commit it's always sql.ErrTxDone, and before one the caller is
// already returning the error that matters.
func rollback(tx *sql.Tx) { _ = tx.Rollback() }

// fromUnix reads a stored time, where 0 means never.
func fromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

// addSessionIP gives a sessions table from before sign-in addresses were
// kept its ip column. CREATE TABLE IF NOT EXISTS leaves an existing table
// as it was, so the column is added here; old rows read as no address.
func addSessionIP(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('sessions')`)
	if err != nil {
		return fmt.Errorf("read sessions columns: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("read sessions columns: %w", err)
		}
		if name == "ip" {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN ip TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add sessions.ip: %w", err)
	}
	return nil
}

// addTailnetLogin gives a users table from before tailnet logins could be
// linked its tailnet_login column, then indexes it. The index is made here
// rather than in schema because on an older database the column only exists
// once this has run. It's partial so any number of users can have no link.
func addTailnetLogin(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('users')`)
	if err != nil {
		return fmt.Errorf("read users columns: %w", err)
	}
	defer rows.Close()
	have := false
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("read users columns: %w", err)
		}
		if name == "tailnet_login" {
			have = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	if !have {
		if _, err := db.Exec(`ALTER TABLE users ADD COLUMN tailnet_login TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add users.tailnet_login: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS users_tailnet_login ON users(tailnet_login) WHERE tailnet_login != ''`); err != nil {
		return fmt.Errorf("index users.tailnet_login: %w", err)
	}
	return nil
}

// addTokenInternet gives an access_tokens table from before tokens could work
// from the internet its internet and internet_used_at columns. Every existing
// token reads as tailnet-only, which is the point: none gains reach it wasn't
// made for.
func addTokenInternet(db *sql.DB) error {
	rows, err := db.Query(`SELECT name FROM pragma_table_info('access_tokens')`)
	if err != nil {
		return fmt.Errorf("read access_tokens columns: %w", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("read access_tokens columns: %w", err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, col := range []string{"internet", "internet_used_at"} {
		if have[col] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE access_tokens ADD COLUMN ` + col + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add access_tokens.%s: %w", col, err)
		}
	}
	return nil
}

// addTokenGrant gives an access_tokens table from before OAuth grants its
// grant_id column, and the index revoking a grant uses. Every existing token
// reads as one made with `outpost token create`, which it was.
func addTokenGrant(db *sql.DB) error {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('access_tokens') WHERE name = 'grant_id'`).Scan(&n); err != nil {
		return fmt.Errorf("read access_tokens columns: %w", err)
	}
	if n == 0 {
		if _, err := db.Exec(`ALTER TABLE access_tokens ADD COLUMN grant_id INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add access_tokens.grant_id: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS access_tokens_grant ON access_tokens(grant_id)`); err != nil {
		return fmt.Errorf("index access_tokens.grant_id: %w", err)
	}
	return nil
}

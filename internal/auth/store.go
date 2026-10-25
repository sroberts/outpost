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
    parent_hash  TEXT NOT NULL DEFAULT '',
    app          TEXT NOT NULL DEFAULT '',
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

// unixOrZero stores the zero time as 0 rather than a large negative number.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// fromUnix is unixOrZero's inverse.
func fromUnix(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

package ai

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays cgo-free
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
    kind       TEXT PRIMARY KEY,
    model      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
    service    TEXT PRIMARY KEY,
    generation INTEGER NOT NULL
);
`

// tokenPrefix marks an app's token for the AI endpoint, so one found in a log
// says what it is, as opat_ does for access tokens.
const tokenPrefix = "oai_"

// Setting is what the portal chose for one kind.
type Setting struct {
	Kind      string
	Model     string
	UpdatedAt time.Time
	UpdatedBy string
}

// store is ai.db: the portal's choice of model for each kind, and what each
// app's token is made from. Like exposure.db it is authoritative: nothing can
// rebuild it, so it is kept apart from state.db, which is only a cache.
//
// A token isn't stored. It is an HMAC of the app's name and generation under
// a key kept here, so the same token can be handed to the app every time it
// starts, including after the daemon restarts, without keeping it in a form
// that could be read back. Regenerating bumps the generation.
type store struct {
	db  *sql.DB
	key []byte
	now func() time.Time

	mu       sync.RWMutex
	settings map[string]Setting
	gens     map[string]int64
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ai dir: %w", err)
	}
	// Owner-only before SQLite sees it, so its -wal and -shm files are too.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create ai db: %w", err)
	}
	_ = f.Close() // only created here; SQLite opens it properly below
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod ai db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open ai db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close() // failing anyway
		return nil, fmt.Errorf("apply ai schema: %w", err)
	}
	s := &store{db: db, now: time.Now}
	if s.key, err = s.tokenKey(); err != nil {
		_ = db.Close() // failing anyway
		return nil, err
	}
	if err := s.reload(); err != nil {
		_ = db.Close() // failing anyway
		return nil, err
	}
	return s, nil
}

func (s *store) close() error { return s.db.Close() }

// tokenKey reads the key tokens are made with, making one the first time.
// INSERT OR IGNORE and a read back, so two daemons racing agree on one.
func (s *store) tokenKey() ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("token key: %w", err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('token_key', ?)`, k); err != nil {
		return nil, fmt.Errorf("save token key: %w", err)
	}
	var got []byte
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'token_key'`).Scan(&got); err != nil {
		return nil, fmt.Errorf("read token key: %w", err)
	}
	if len(got) != 32 {
		return nil, errors.New("ai.db's token key is damaged; restore ai.db from a backup, or delete it to start over with new tokens and no model chosen")
	}
	return got, nil
}

func (s *store) reload() error {
	settings := map[string]Setting{}
	rows, err := s.db.Query(`SELECT kind, model, updated_at, updated_by FROM settings`)
	if err != nil {
		return fmt.Errorf("read ai settings: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only
	for rows.Next() {
		var st Setting
		var at int64
		if err := rows.Scan(&st.Kind, &st.Model, &at, &st.UpdatedBy); err != nil {
			return err
		}
		st.UpdatedAt = time.Unix(at, 0)
		settings[st.Kind] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}

	gens := map[string]int64{}
	trows, err := s.db.Query(`SELECT service, generation FROM tokens`)
	if err != nil {
		return fmt.Errorf("read ai tokens: %w", err)
	}
	defer func() { _ = trows.Close() }() // read-only
	for trows.Next() {
		var name string
		var gen int64
		if err := trows.Scan(&name, &gen); err != nil {
			return err
		}
		gens[name] = gen
	}
	if err := trows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.settings, s.gens = settings, gens
	s.mu.Unlock()
	return nil
}

func (s *store) get(kind string) (Setting, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.settings[kind]
	return st, ok
}

func (s *store) set(kind, model, by string) error {
	if _, err := s.db.Exec(`INSERT INTO settings (kind, model, updated_at, updated_by) VALUES (?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET model = excluded.model, updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		kind, model, s.now().Unix(), by); err != nil {
		return fmt.Errorf("save ai setting: %w", err)
	}
	return s.reload()
}

func (s *store) reset(kind string) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE kind = ?`, kind); err != nil {
		return fmt.Errorf("reset ai setting: %w", err)
	}
	return s.reload()
}

// generation is the app's token generation, 0 until it is first regenerated.
func (s *store) generation(service string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gens[service]
}

// regenerate gives service a new token; the old one stops working.
func (s *store) regenerate(service string) error {
	if _, err := s.db.Exec(`INSERT INTO tokens (service, generation) VALUES (?, 1)
		ON CONFLICT(service) DO UPDATE SET generation = generation + 1`, service); err != nil {
		return fmt.Errorf("regenerate token: %w", err)
	}
	return s.reload()
}

// token is service's token at its current generation.
func (s *store) token(service string) (tok string, gen int64) {
	gen = s.generation(service)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(service))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(gen, 10)))
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), gen
}

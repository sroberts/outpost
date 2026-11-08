package ai

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays cgo-free

	"github.com/sroberts/outpost/internal/apptoken"
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
CREATE TABLE IF NOT EXISTS access (
    kind       TEXT PRIMARY KEY,
    every      INTEGER NOT NULL,
    apps       TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL
);
`

// migrations bring an ai.db from an earlier release up to date. Each is
// checked for before it's run, so running them again is harmless.
var migrations = []struct {
	table, column, add string
}{
	// v0.5.0 had only Ollama, so every setting it saved is Ollama's.
	{"settings", "provider", `ALTER TABLE settings ADD COLUMN provider TEXT NOT NULL DEFAULT 'ollama'`},
}

// tokenPrefix marks an app's token for the AI endpoint, so one found in a log
// says what it is, as opat_ does for access tokens.
const tokenPrefix = "oai_"

// Setting is what the portal chose for one kind.
type Setting struct {
	Kind      string
	Provider  string
	Model     string
	UpdatedAt time.Time
	UpdatedBy string
}

// Access is which apps the portal let use one kind: Every, or Apps.
type Access struct {
	Kind      string
	Every     bool
	Apps      []string
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
	access   map[string]Access
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
	if err := migrate(db); err != nil {
		_ = db.Close() // failing anyway
		return nil, err
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

func migrate(db *sql.DB) error {
	for _, m := range migrations {
		rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, m.table)
		if err != nil {
			return fmt.Errorf("read ai.db's %s table: %w", m.table, err)
		}
		has := false
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				_ = rows.Close() // failing anyway
				return err
			}
			has = has || name == m.column
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if !has {
			if _, err := db.Exec(m.add); err != nil {
				return fmt.Errorf("upgrade ai.db: %w", err)
			}
		}
	}
	return nil
}

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
	rows, err := s.db.Query(`SELECT kind, provider, model, updated_at, updated_by FROM settings`)
	if err != nil {
		return fmt.Errorf("read ai settings: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only
	for rows.Next() {
		var st Setting
		var at int64
		if err := rows.Scan(&st.Kind, &st.Provider, &st.Model, &at, &st.UpdatedBy); err != nil {
			return err
		}
		st.UpdatedAt = time.Unix(at, 0)
		settings[st.Kind] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}

	access := map[string]Access{}
	arows, err := s.db.Query(`SELECT kind, every, apps, updated_at, updated_by FROM access`)
	if err != nil {
		return fmt.Errorf("read ai access: %w", err)
	}
	defer func() { _ = arows.Close() }() // read-only
	for arows.Next() {
		var a Access
		var every int
		var apps string
		var at int64
		if err := arows.Scan(&a.Kind, &every, &apps, &at, &a.UpdatedBy); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(apps), &a.Apps); err != nil {
			return fmt.Errorf("ai access for %s: %w", a.Kind, err)
		}
		a.Every, a.UpdatedAt = every != 0, time.Unix(at, 0)
		access[a.Kind] = a
	}
	if err := arows.Err(); err != nil {
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
	s.settings, s.access, s.gens = settings, access, gens
	s.mu.Unlock()
	return nil
}

func (s *store) get(kind string) (Setting, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.settings[kind]
	return st, ok
}

func (s *store) set(kind, provider, model, by string) error {
	if _, err := s.db.Exec(`INSERT INTO settings (kind, provider, model, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET provider = excluded.provider, model = excluded.model,
		updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		kind, provider, model, s.now().Unix(), by); err != nil {
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

func (s *store) getAccess(kind string) (Access, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.access[kind]
	a.Apps = append([]string(nil), a.Apps...)
	return a, ok
}

func (s *store) setAccess(kind string, every bool, apps []string, by string) error {
	if apps == nil {
		apps = []string{}
	}
	b, err := json.Marshal(apps)
	if err != nil {
		return err
	}
	e := 0
	if every {
		e = 1
	}
	if _, err := s.db.Exec(`INSERT INTO access (kind, every, apps, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(kind) DO UPDATE SET every = excluded.every, apps = excluded.apps,
		updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		kind, e, string(b), s.now().Unix(), by); err != nil {
		return fmt.Errorf("save ai access: %w", err)
	}
	return s.reload()
}

func (s *store) resetAccess(kind string) error {
	if _, err := s.db.Exec(`DELETE FROM access WHERE kind = ?`, kind); err != nil {
		return fmt.Errorf("reset ai access: %w", err)
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
	return apptoken.Mint(tokenPrefix, s.key, service, gen), gen
}

// Package exposure keeps what the portal has set about how an app is reached
// without a login: its public paths, and whether Tailscale Funnel answers
// them from the internet. A setting here wins over the manifest's funnel and
// public_paths; an app with none uses the manifest's.
//
// Like auth.db and secrets.db, exposure.db is authoritative: nothing can
// rebuild it, so it is kept apart from state.db, which is only a cache.
package exposure

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays cgo-free
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
    service      TEXT PRIMARY KEY,
    funnel       INTEGER NOT NULL,
    public_paths TEXT NOT NULL,
    updated_at   INTEGER NOT NULL,
    updated_by   TEXT NOT NULL
);
`

// Setting is what the portal set for one app.
type Setting struct {
	Service     string
	Funnel      bool
	PublicPaths []string
	UpdatedAt   time.Time
	UpdatedBy   string
}

// Store is the settings database. It is safe for concurrent use, and keeps
// every setting in memory, since the supervisor reads them on each apply.
type Store struct {
	db  *sql.DB
	now func() time.Time

	mu    sync.RWMutex
	cache map[string]Setting
}

// Open opens, creating if needed, the database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create exposure dir: %w", err)
	}
	// Owner-only before SQLite sees it, so its -wal and -shm files are too.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create exposure db: %w", err)
	}
	_ = f.Close() // only created here; SQLite opens it properly below
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod exposure db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open exposure db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close() // failing anyway
		return nil, fmt.Errorf("apply exposure schema: %w", err)
	}
	s := &Store{db: db, now: time.Now}
	if err := s.reload(); err != nil {
		_ = db.Close() // failing anyway
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT service, funnel, public_paths, updated_at, updated_by FROM settings`)
	if err != nil {
		return fmt.Errorf("read exposure settings: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only
	cache := map[string]Setting{}
	for rows.Next() {
		var st Setting
		var funnel int
		var paths string
		var at int64
		if err := rows.Scan(&st.Service, &funnel, &paths, &at, &st.UpdatedBy); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(paths), &st.PublicPaths); err != nil {
			return fmt.Errorf("exposure setting for %s: %w", st.Service, err)
		}
		st.Funnel, st.UpdatedAt = funnel != 0, time.Unix(at, 0)
		cache[st.Service] = st
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache = cache
	s.mu.Unlock()
	return nil
}

// Get returns the setting for service, and false if the portal has set none.
func (s *Store) Get(service string) (Setting, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.cache[service]
	st.PublicPaths = append([]string(nil), st.PublicPaths...)
	return st, ok
}

// Override is Get as the supervisor reads it.
func (s *Store) Override(service string) (funnel bool, publicPaths []string, ok bool) {
	st, ok := s.Get(service)
	return st.Funnel, st.PublicPaths, ok
}

// List returns every setting, by service.
func (s *Store) List() []Setting {
	s.mu.RLock()
	out := make([]Setting, 0, len(s.cache))
	for _, st := range s.cache {
		st.PublicPaths = append([]string(nil), st.PublicPaths...)
		out = append(out, st)
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

// Set records the portal's setting for service, replacing any before it.
// The caller has checked it against the manifest's rules.
func (s *Store) Set(service string, funnel bool, publicPaths []string, by string) error {
	if service == "" {
		return fmt.Errorf("a service is required")
	}
	if publicPaths == nil {
		publicPaths = []string{}
	}
	paths, err := json.Marshal(publicPaths)
	if err != nil {
		return err
	}
	f := 0
	if funnel {
		f = 1
	}
	if _, err := s.db.Exec(`INSERT INTO settings (service, funnel, public_paths, updated_at, updated_by) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(service) DO UPDATE SET funnel = excluded.funnel, public_paths = excluded.public_paths,
		updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
		service, f, string(paths), s.now().Unix(), by); err != nil {
		return fmt.Errorf("save exposure setting: %w", err)
	}
	return s.reload()
}

// Reset forgets the portal's setting for service, so the manifest's applies
// again.
func (s *Store) Reset(service string) error {
	if _, err := s.db.Exec(`DELETE FROM settings WHERE service = ?`, service); err != nil {
		return fmt.Errorf("reset exposure setting: %w", err)
	}
	return s.reload()
}

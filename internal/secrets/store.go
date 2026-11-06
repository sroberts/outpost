// Package secrets keeps the values services are given as environment
// variables (API keys and the like) out of their manifests, in a store an
// admin can rotate from the portal or the CLI.
//
// A secret has a name, the environment variable it's handed over as, and a
// value. It reaches a service through a binding: to that service by name, or
// to every service. A service-bound secret wins over a global one for the
// same variable, so one app can be given its own key while the rest share one.
//
// secrets.db is, like auth.db, authoritative: nothing can rebuild it, which is
// why it is its own file rather than part of state.db. Values are sealed with
// AES-256-GCM under a key kept beside it in secrets.key, so a copy of the
// database on its own (a stray backup, a file attached to a bug report)
// doesn't give them away. Anyone who can read both files as the operator can
// read every value, and so can every service, which runs as the operator
// too: this is for keeping keys out of manifests and making rotation easy,
// not a boundary between local processes.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS secrets (
    name       TEXT PRIMARY KEY,
    env        TEXT NOT NULL,
    sealed     BLOB NOT NULL,
    revision   INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

-- service '' binds the secret to every service.
CREATE TABLE IF NOT EXISTS bindings (
    secret  TEXT NOT NULL REFERENCES secrets(name) ON DELETE CASCADE,
    service TEXT NOT NULL,
    PRIMARY KEY (secret, service)
);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
`

// keySize is AES-256's.
const keySize = 32

// Limits on what can be stored.
const (
	MaxValue   = 64 << 10
	maxService = 128
)

var (
	nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	envRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
)

// reservedEnv are variables a secret may not set: changing them would break
// how a service is started or found, rather than hand it a credential.
var reservedEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
	"SHELL": true, "PWD": true, "TMPDIR": true,
}

// Errors a caller may want to tell apart.
var (
	ErrNotFound = errors.New("no such secret")
	ErrExists   = errors.New("a secret with that name already exists")
)

// Secret is a secret as anyone outside this package sees it: never its value.
type Secret struct {
	Name      string
	Env       string
	Revision  int64
	CreatedAt time.Time
	UpdatedAt time.Time
	// Global is whether it's bound to every service.
	Global bool
	// Services are the services it's bound to by name, sorted.
	Services []string
}

// entry is a secret with its value, in memory only.
type entry struct {
	Secret
	value string
}

// Store is the secrets database. It is safe for concurrent use. It keeps every
// secret in memory, decrypted, so the reconcile loop can ask about a service
// on every tick without touching the disk.
type Store struct {
	db   *sql.DB
	aead cipher.AEAD
	now  func() time.Time

	mu    sync.RWMutex
	cache map[string]*entry
}

// Open opens (creating if needed) the database at path and its key at
// keyPath. A database with sealed values and no key is refused rather than
// given a new key, which would lose them for good.
func Open(path, keyPath string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create secrets dir: %w", err)
	}
	// Owner-only before SQLite sees it, as auth.db is: SQLite gives the -wal
	// and -shm files the main file's mode.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create secrets db: %w", err)
	}
	f.Close()
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod secrets db: %w", err)
	}

	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open secrets db: %w", err)
	}
	// One connection, so each check-then-write (a binding's clash check) is
	// serialised without depending on SQLite lock upgrades.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply secrets schema: %w", err)
	}
	var sealed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM secrets`).Scan(&sealed); err != nil {
		db.Close()
		return nil, fmt.Errorf("read secrets db: %w", err)
	}
	key, err := loadKey(keyPath, sealed > 0)
	if err != nil {
		db.Close()
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		db.Close()
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, aead: aead, now: time.Now}
	if err := s.reload(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// loadKey reads the key, or makes one when there's nothing it could have
// sealed.
func loadKey(path string, needed bool) ([]byte, error) {
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(b) != keySize {
			return nil, fmt.Errorf("%s is %d bytes, not a %d-byte key", path, len(b), keySize)
		}
		// Tighten a key left readable by others; the mode is all that guards it.
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("chmod %s: %w", path, err)
		}
		return b, nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("read secrets key: %w", err)
	case needed:
		return nil, fmt.Errorf("%s is missing, and the secrets in the database can't be read without it; restore it from a backup, or move the database aside to start again", path)
	}
	key := make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("make secrets key: %w", err)
	}
	// O_EXCL so two daemons starting at once can't each write their own.
	kf, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("write secrets key: %w", err)
	}
	if _, err := kf.Write(key); err != nil {
		kf.Close()
		_ = os.Remove(path) // a short key would be refused next time anyway
		return nil, fmt.Errorf("write secrets key: %w", err)
	}
	if err := kf.Close(); err != nil {
		return nil, fmt.Errorf("write secrets key: %w", err)
	}
	return key, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// seal encrypts a value, bound to the secret's name so a sealed value can't
// be moved onto another secret in the database.
func (s *Store) seal(name, value string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(value), []byte(name)), nil
}

func (s *Store) open(name string, sealed []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return "", fmt.Errorf("secret %s is malformed", name)
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %s can't be decrypted with this secrets.key", name)
	}
	return string(plain), nil
}

// reload rebuilds the cache from the database. It runs after every write, so
// the cache never shows a change the database didn't take.
func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT name, env, sealed, revision, created_at, updated_at FROM secrets`)
	if err != nil {
		return fmt.Errorf("read secrets: %w", err)
	}
	cache := map[string]*entry{}
	for rows.Next() {
		var (
			e                entry
			sealed           []byte
			created, updated int64
		)
		if err := rows.Scan(&e.Name, &e.Env, &sealed, &e.Revision, &created, &updated); err != nil {
			rows.Close()
			return fmt.Errorf("read secrets: %w", err)
		}
		v, err := s.open(e.Name, sealed)
		if err != nil {
			rows.Close()
			return err
		}
		e.value = v
		e.CreatedAt, e.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
		cache[e.Name] = &e
	}
	if err := rows.Close(); err != nil {
		return err
	}
	brows, err := s.db.Query(`SELECT secret, service FROM bindings ORDER BY secret, service`)
	if err != nil {
		return fmt.Errorf("read bindings: %w", err)
	}
	defer brows.Close()
	for brows.Next() {
		var secret, service string
		if err := brows.Scan(&secret, &service); err != nil {
			return fmt.Errorf("read bindings: %w", err)
		}
		e, ok := cache[secret]
		if !ok {
			continue
		}
		if service == "" {
			e.Global = true
		} else {
			e.Services = append(e.Services, service)
		}
	}
	if err := brows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.cache = cache
	s.mu.Unlock()
	return nil
}

// ValidName checks a secret's name: lower case, as a service's is.
func ValidName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("secret name %q must be 1-64 characters of a-z, 0-9, '.', '_' and '-', starting with a letter or digit", name)
	}
	return nil
}

// DefaultEnv is the variable a secret is handed over as when none is given:
// its name in upper case, with '-' and '.' as '_'.
func DefaultEnv(name string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '-' || r == '.':
			return '_'
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		}
		return r
	}, name)
}

// ValidEnv checks the variable a secret is handed over as.
func ValidEnv(env string) error {
	switch {
	case !envRE.MatchString(env):
		return fmt.Errorf("variable %q must be letters, digits and '_', not starting with a digit", env)
	case strings.HasPrefix(strings.ToUpper(env), "OUTPOST_"):
		return fmt.Errorf("variable %q: OUTPOST_ variables are set by Outpost itself", env)
	case reservedEnv[strings.ToUpper(env)]:
		return fmt.Errorf("variable %q is how a process is started and found, not a credential", env)
	}
	return nil
}

// ValidValue checks a secret's value. A NUL can't be passed in the
// environment at all.
func ValidValue(v string) error {
	switch {
	case v == "":
		return errors.New("the value is empty")
	case len(v) > MaxValue:
		return fmt.Errorf("the value is %d bytes; the most is %d", len(v), MaxValue)
	case strings.ContainsRune(v, 0):
		return errors.New("the value contains a NUL byte, which can't be passed in an environment variable")
	case !utf8.ValidString(v):
		return errors.New("the value isn't valid UTF-8")
	}
	return nil
}

func validService(service string) error {
	if service == "" || len(service) > maxService || strings.ContainsFunc(service, unicode.IsControl) {
		return fmt.Errorf("%q isn't a service name", service)
	}
	return nil
}

// Create adds a secret, bound to nothing yet.
func (s *Store) Create(name, env, value string) (Secret, error) {
	if err := ValidName(name); err != nil {
		return Secret{}, err
	}
	if err := ValidEnv(env); err != nil {
		return Secret{}, err
	}
	if err := ValidValue(value); err != nil {
		return Secret{}, err
	}
	sealed, err := s.seal(name, value)
	if err != nil {
		return Secret{}, err
	}
	now := s.now().Unix()
	res, err := s.db.Exec(`INSERT INTO secrets (name, env, sealed, revision, created_at, updated_at)
		VALUES (?, ?, ?, 1, ?, ?) ON CONFLICT(name) DO NOTHING`, name, env, sealed, now, now)
	if err != nil {
		return Secret{}, fmt.Errorf("create secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Secret{}, ErrExists
	}
	return s.after(name)
}

// Rotate replaces a secret's value. Every service it reaches is restarted
// with the new one by the supervisor, which sees the revision change.
func (s *Store) Rotate(name, value string) (Secret, error) {
	if err := ValidValue(value); err != nil {
		return Secret{}, err
	}
	sealed, err := s.seal(name, value)
	if err != nil {
		return Secret{}, err
	}
	res, err := s.db.Exec(`UPDATE secrets SET sealed = ?, revision = revision + 1, updated_at = ? WHERE name = ?`,
		sealed, s.now().Unix(), name)
	if err != nil {
		return Secret{}, fmt.Errorf("rotate secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Secret{}, ErrNotFound
	}
	return s.after(name)
}

// Delete removes a secret and its bindings, and reports the services that
// had it, which lose it at their next restart (made at once by the supervisor).
func (s *Store) Delete(name string) (Secret, error) {
	before, ok := s.Get(name)
	if !ok {
		return Secret{}, ErrNotFound
	}
	res, err := s.db.Exec(`DELETE FROM secrets WHERE name = ?`, name)
	if err != nil {
		return Secret{}, fmt.Errorf("delete secret: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Secret{}, ErrNotFound
	}
	return before, s.reload()
}

// Bind hands the secret to service, or to every service when service is "".
// Two secrets for the same variable can't be bound at the same level: which
// one a service got would be a coin toss.
func (s *Store) Bind(name, service string) (Secret, error) {
	if service != "" {
		if err := validService(service); err != nil {
			return Secret{}, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Secret{}, err
	}
	defer func() { _ = tx.Rollback() }() // a no-op once committed

	var env string
	if err := tx.QueryRow(`SELECT env FROM secrets WHERE name = ?`, name).Scan(&env); errors.Is(err, sql.ErrNoRows) {
		return Secret{}, ErrNotFound
	} else if err != nil {
		return Secret{}, err
	}
	var clash string
	err = tx.QueryRow(`SELECT s.name FROM bindings b JOIN secrets s ON s.name = b.secret
		WHERE b.service = ? AND s.env = ? AND s.name != ? LIMIT 1`, service, env, name).Scan(&clash)
	switch {
	case err == nil:
		where := "every service"
		if service != "" {
			where = service
		}
		return Secret{}, fmt.Errorf("%s already gets %s from secret %s; unbind that first", where, env, clash)
	case !errors.Is(err, sql.ErrNoRows):
		return Secret{}, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO bindings (secret, service) VALUES (?, ?)`, name, service); err != nil {
		return Secret{}, fmt.Errorf("bind secret: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Secret{}, err
	}
	return s.after(name)
}

// Unbind takes the secret away from service, or from every service when
// service is "" (its by-name bindings stay).
func (s *Store) Unbind(name, service string) (Secret, error) {
	if _, ok := s.Get(name); !ok {
		return Secret{}, ErrNotFound
	}
	if _, err := s.db.Exec(`DELETE FROM bindings WHERE secret = ? AND service = ?`, name, service); err != nil {
		return Secret{}, fmt.Errorf("unbind secret: %w", err)
	}
	return s.after(name)
}

// after reloads the cache after a write and returns the secret as it now is.
func (s *Store) after(name string) (Secret, error) {
	if err := s.reload(); err != nil {
		return Secret{}, err
	}
	sec, ok := s.Get(name)
	if !ok {
		return Secret{}, ErrNotFound
	}
	return sec, nil
}

// Get returns a secret by name, without its value.
func (s *Store) Get(name string) (Secret, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.cache[name]
	if !ok {
		return Secret{}, false
	}
	return e.public(), true
}

func (e *entry) public() Secret {
	sec := e.Secret
	sec.Services = append([]string(nil), e.Services...)
	return sec
}

// List returns every secret, by name, without values.
func (s *Store) List() []Secret {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Secret, 0, len(s.cache))
	for _, e := range s.cache {
		out = append(out, e.public())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Resolve returns the variables service is given, and a revision that changes
// whenever any of them would: a value rotated, a binding added or removed. The
// revision is a hash of names and counters, never of a value, so it can be
// logged or stored.
func (s *Store) Resolve(service string) (map[string]string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	picked := s.pickLocked(service)
	if len(picked) == 0 {
		return nil, ""
	}
	envs := make([]string, 0, len(picked))
	for env := range picked {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	h := sha256.New()
	vars := make(map[string]string, len(picked))
	for _, env := range envs {
		e := picked[env]
		vars[env] = e.value
		fmt.Fprintf(h, "%s=%s@%d\x00", env, e.Name, e.Revision)
	}
	return vars, fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// Reaches returns the services, among those given, that get the secret: the
// ones a change to it restarts. A service whose own secret shadows a global
// one for the same variable isn't counted.
func (s *Store) Reaches(name string, services []string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, svc := range services {
		for _, e := range s.pickLocked(svc) {
			if e.Name == name {
				out = append(out, svc)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// pickLocked is the secret service gets for each variable. One bound to it by
// name wins over a global one; Bind keeps there from being two at one level.
func (s *Store) pickLocked(service string) map[string]*entry {
	picked := map[string]*entry{}
	named := map[string]bool{}
	for _, e := range s.cache {
		isNamed := false
		for _, svc := range e.Services {
			if svc == service {
				isNamed = true
				break
			}
		}
		if !isNamed && !e.Global {
			continue
		}
		if _, ok := picked[e.Env]; ok && (named[e.Env] || !isNamed) {
			continue
		}
		picked[e.Env], named[e.Env] = e, isNamed
	}
	return picked
}

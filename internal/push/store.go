package push

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays cgo-free
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS subscriptions (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL,
    sign_in       TEXT NOT NULL,
    endpoint      TEXT NOT NULL UNIQUE,
    p256dh        TEXT NOT NULL,
    auth          TEXT NOT NULL,
    user_agent    TEXT NOT NULL,
    platform      TEXT NOT NULL,
    created_at    INTEGER NOT NULL,
    last_success  INTEGER NOT NULL DEFAULT 0,
    failure_count INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS subscriptions_user ON subscriptions (username);
CREATE TABLE IF NOT EXISTS mutes (
    username TEXT NOT NULL,
    service  TEXT NOT NULL,
    PRIMARY KEY (username, service)
);
`

// Subscription is one device's subscription, as a browser made it. Keys are
// credentials: anyone holding them and the endpoint can send to the device,
// so they are never logged or shown.
type Subscription struct {
	ID       string
	Username string
	// SignIn is the sign-in it was made from; when that ends, so does this.
	SignIn    string
	Endpoint  string
	P256dh    string
	Auth      string
	UserAgent string
	Platform  string
	CreatedAt time.Time
	// LastSuccess is the last push the service took for it, zero if none.
	LastSuccess  time.Time
	FailureCount int
}

// store is push.db: the VAPID key and the subscriptions. Neither can be
// rebuilt from anything else (a new key strands every subscription), so,
// like auth.db, it is kept apart from state.db.
type store struct {
	db  *sql.DB
	now func() time.Time
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create push dir: %w", err)
	}
	// Owner-only before SQLite sees it, so its -wal and -shm files are too.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create push db: %w", err)
	}
	_ = f.Close() // only created here; SQLite opens it properly below
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod push db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open push db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close() // failing anyway
		return nil, fmt.Errorf("apply push schema: %w", err)
	}
	return &store{db: db, now: time.Now}, nil
}

func (s *store) close() error { return s.db.Close() }

// vapidKey reads the key, making one the first time. INSERT OR IGNORE and a
// read back, so two daemons racing agree on one.
func (s *store) vapidKey() (vapidKey, error) {
	k, err := newVAPIDKey()
	if err != nil {
		return vapidKey{}, err
	}
	raw, err := k.raw()
	if err != nil {
		return vapidKey{}, err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES ('vapid_key', ?)`, raw); err != nil {
		return vapidKey{}, fmt.Errorf("save the VAPID key: %w", err)
	}
	var got []byte
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = 'vapid_key'`).Scan(&got); err != nil {
		return vapidKey{}, fmt.Errorf("read the VAPID key: %w", err)
	}
	k, err = parseVAPIDKey(got)
	if err != nil {
		return vapidKey{}, errors.New("push.db's VAPID key is damaged; restore push.db from a backup, or delete it and have everyone turn notifications on again")
	}
	return k, nil
}

// randomKey reads a 32-byte key, making one the first time.
func (s *store) randomKey(name string) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?)`, name, k); err != nil {
		return nil, fmt.Errorf("save %s: %w", name, err)
	}
	var got []byte
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, name).Scan(&got); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(got) != 32 {
		return nil, fmt.Errorf("push.db's %s is damaged; restore push.db from a backup, or delete it to start over", name)
	}
	return got, nil
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b) // crypto/rand doesn't fail
	return hex.EncodeToString(b)
}

// upsert keeps sub, replacing whatever had its endpoint: a browser hands the
// same endpoint back when asked again, and it may now be another sign-in's,
// or even another user's on a shared machine.
func (s *store) upsert(sub Subscription) (string, error) {
	id := newID()
	err := s.db.QueryRow(`INSERT INTO subscriptions
	(id, username, sign_in, endpoint, p256dh, auth, user_agent, platform, created_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(endpoint) DO UPDATE SET
	    username = excluded.username, sign_in = excluded.sign_in,
	    p256dh = excluded.p256dh, auth = excluded.auth,
	    user_agent = excluded.user_agent, platform = excluded.platform,
	    failure_count = 0
	RETURNING id`,
		id, sub.Username, sub.SignIn, sub.Endpoint, sub.P256dh, sub.Auth, sub.UserAgent, sub.Platform,
		s.now().Unix()).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("save subscription: %w", err)
	}
	return id, nil
}

const subCols = `id, username, sign_in, endpoint, p256dh, auth, user_agent, platform, created_at, last_success, failure_count`

func scanSubs(rows *sql.Rows) ([]Subscription, error) {
	defer func() { _ = rows.Close() }() // read-only
	var out []Subscription
	for rows.Next() {
		var sub Subscription
		var created, last int64
		if err := rows.Scan(&sub.ID, &sub.Username, &sub.SignIn, &sub.Endpoint, &sub.P256dh, &sub.Auth,
			&sub.UserAgent, &sub.Platform, &created, &last, &sub.FailureCount); err != nil {
			return nil, err
		}
		sub.CreatedAt = time.Unix(created, 0)
		if last > 0 {
			sub.LastSuccess = time.Unix(last, 0)
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// ofUser lists username's subscriptions, oldest first.
func (s *store) ofUser(username string) ([]Subscription, error) {
	rows, err := s.db.Query(`SELECT `+subCols+` FROM subscriptions WHERE username = ? ORDER BY created_at, id`, username)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// users lists everyone with a subscription.
func (s *store) users() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT username FROM subscriptions ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// remove forgets one subscription by id, if it is username's; "" for
// username removes it whoever's it is.
func (s *store) remove(username, id string) (bool, error) {
	var res sql.Result
	var err error
	if username == "" {
		res, err = s.db.Exec(`DELETE FROM subscriptions WHERE id = ?`, id)
	} else {
		res, err = s.db.Exec(`DELETE FROM subscriptions WHERE id = ? AND username = ?`, id, username)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected() // SQLite always reports it
	return n > 0, nil
}

// removeEndpoint forgets username's subscription at endpoint.
func (s *store) removeEndpoint(username, endpoint string) error {
	_, err := s.db.Exec(`DELETE FROM subscriptions WHERE endpoint = ? AND username = ?`, endpoint, username)
	return err
}

func (s *store) succeeded(id string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET last_success = ?, failure_count = 0 WHERE id = ?`, s.now().Unix(), id)
	return err
}

func (s *store) failed(id string) error {
	_, err := s.db.Exec(`UPDATE subscriptions SET failure_count = failure_count + 1 WHERE id = ?`, id)
	return err
}

// muted lists the senders username has turned off.
func (s *store) muted(username string) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT service FROM mutes WHERE username = ?`, username)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }() // read-only
	out := map[string]bool{}
	for rows.Next() {
		var svc string
		if err := rows.Scan(&svc); err != nil {
			return nil, err
		}
		out[svc] = true
	}
	return out, rows.Err()
}

func (s *store) setMuted(username, service string, muted bool) error {
	var err error
	if muted {
		_, err = s.db.Exec(`INSERT OR IGNORE INTO mutes (username, service) VALUES (?, ?)`, username, service)
	} else {
		_, err = s.db.Exec(`DELETE FROM mutes WHERE username = ? AND service = ?`, username, service)
	}
	return err
}

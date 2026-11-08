package mail

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
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure Go, so the binary stays cgo-free
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
    service    TEXT PRIMARY KEY,
    generation INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS aliases (
    alias      TEXT PRIMARY KEY,
    service    TEXT NOT NULL,
    note       TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    created_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS senders (
    service    TEXT PRIMARY KEY,
    updated_at INTEGER NOT NULL,
    updated_by TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS messages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    src         TEXT NOT NULL UNIQUE,
    service     TEXT NOT NULL,
    route       TEXT NOT NULL,
    rcpt        TEXT NOT NULL,
    from_name   TEXT NOT NULL,
    from_addr   TEXT NOT NULL,
    reply_to    TEXT NOT NULL,
    subject     TEXT NOT NULL,
    message_id  TEXT NOT NULL,
    date        INTEGER NOT NULL,
    received    INTEGER NOT NULL,
    text        TEXT NOT NULL,
    html        TEXT NOT NULL,
    attachments INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_service ON messages (service, id);
CREATE INDEX IF NOT EXISTS messages_route ON messages (route, id);
CREATE TABLE IF NOT EXISTS sent (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    service    TEXT NOT NULL,
    route      TEXT NOT NULL,
    at         INTEGER NOT NULL,
    rcpts      TEXT NOT NULL,
    subject    TEXT NOT NULL,
    message_id TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS sent_service ON sent (service, at);
`

// tokenPrefix marks an app's token for the mail endpoint, so one found in a
// log says what it is.
const tokenPrefix = "omail_"

// keep is how many messages each app's inbox holds; the oldest go first.
const keep = 500

// alias is a random route given to an app, kept in mail.db.
type alias struct {
	Alias, Service, Note string
	CreatedAt            time.Time
	CreatedBy            string
}

// store is mail.db: the routes and who may send, which nothing else can
// rebuild, and the mail held for apps. Like ai.db it is kept apart from
// state.db, which is only a cache.
type store struct {
	db      *sql.DB
	key     []byte // makes app tokens
	feedKey []byte // makes feed links
	now     func() time.Time

	mu      sync.RWMutex
	gens    map[string]int64
	aliases map[string]alias // by alias
	senders map[string]bool
}

func openStore(path string) (*store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create mail dir: %w", err)
	}
	// Owner-only before SQLite sees it, so its -wal and -shm files are too.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create mail db: %w", err)
	}
	_ = f.Close() // only created here; SQLite opens it properly below
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod mail db: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open mail db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close() // failing anyway
		return nil, fmt.Errorf("apply mail schema: %w", err)
	}
	s := &store{db: db, now: time.Now}
	if s.key, err = s.randomKey("token_key"); err == nil {
		s.feedKey, err = s.randomKey("feed_key")
	}
	if err == nil {
		err = s.reload()
	}
	if err != nil {
		_ = db.Close() // failing anyway
		return nil, err
	}
	return s, nil
}

func (s *store) close() error { return s.db.Close() }

// randomKey reads a key, making one the first time. INSERT OR IGNORE and a
// read back, so two daemons racing agree on one.
func (s *store) randomKey(name string) ([]byte, error) {
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO meta (key, value) VALUES (?, ?)`, name, k); err != nil {
		return nil, fmt.Errorf("save %s: %w", name, err)
	}
	var got []byte
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, name).Scan(&got); err != nil {
		return nil, fmt.Errorf("read %s: %w", name, err)
	}
	if len(got) != 32 {
		return nil, fmt.Errorf("mail.db's %s is damaged; restore mail.db from a backup, or delete it to start over", name)
	}
	return got, nil
}

func (s *store) reload() error {
	gens := map[string]int64{}
	rows, err := s.db.Query(`SELECT service, generation FROM tokens`)
	if err != nil {
		return fmt.Errorf("read mail tokens: %w", err)
	}
	defer func() { _ = rows.Close() }() // read-only
	for rows.Next() {
		var name string
		var gen int64
		if err := rows.Scan(&name, &gen); err != nil {
			return err
		}
		gens[name] = gen
	}
	if err := rows.Err(); err != nil {
		return err
	}

	aliases := map[string]alias{}
	arows, err := s.db.Query(`SELECT alias, service, note, created_at, created_by FROM aliases`)
	if err != nil {
		return fmt.Errorf("read mail aliases: %w", err)
	}
	defer func() { _ = arows.Close() }() // read-only
	for arows.Next() {
		var a alias
		var at int64
		if err := arows.Scan(&a.Alias, &a.Service, &a.Note, &at, &a.CreatedBy); err != nil {
			return err
		}
		a.CreatedAt = time.Unix(at, 0)
		aliases[a.Alias] = a
	}
	if err := arows.Err(); err != nil {
		return err
	}

	senders := map[string]bool{}
	srows, err := s.db.Query(`SELECT service FROM senders`)
	if err != nil {
		return fmt.Errorf("read mail senders: %w", err)
	}
	defer func() { _ = srows.Close() }() // read-only
	for srows.Next() {
		var name string
		if err := srows.Scan(&name); err != nil {
			return err
		}
		senders[name] = true
	}
	if err := srows.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	s.gens, s.aliases, s.senders = gens, aliases, senders
	s.mu.Unlock()
	return nil
}

// meta reads a setting kept as text; "" when unset.
func (s *store) meta(key string) string {
	var v []byte
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`, key).Scan(&v); err != nil {
		return ""
	}
	return string(v)
}

func (s *store) setMeta(key, value string) error {
	if value == "" {
		_, err := s.db.Exec(`DELETE FROM meta WHERE key = ?`, key)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, []byte(value))
	return err
}

// cursor is where reading the inbox got to: the mailbox's UIDVALIDITY and
// the highest UID looked at under it.
func (s *store) cursor() (validity, last uint32) {
	v, _ := strconv.ParseUint(s.meta("uidvalidity"), 10, 32)
	l, _ := strconv.ParseUint(s.meta("last_uid"), 10, 32)
	return uint32(v), uint32(l)
}

func (s *store) setCursor(validity, last uint32) error {
	if err := s.setMeta("uidvalidity", strconv.FormatUint(uint64(validity), 10)); err != nil {
		return err
	}
	return s.setMeta("last_uid", strconv.FormatUint(uint64(last), 10))
}

// forgetCursor starts the next read afresh, as for a new account.
func (s *store) forgetCursor() error {
	if err := s.setMeta("uidvalidity", ""); err != nil {
		return err
	}
	return s.setMeta("last_uid", "")
}

func (s *store) generation(service string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gens[service]
}

func (s *store) regenerate(service string) error {
	if _, err := s.db.Exec(`INSERT INTO tokens (service, generation) VALUES (?, 1)
		ON CONFLICT(service) DO UPDATE SET generation = generation + 1`, service); err != nil {
		return fmt.Errorf("regenerate token: %w", err)
	}
	return s.reload()
}

// token is service's token at its current generation: an HMAC, so it needn't
// be stored to be handed out the same each time.
func (s *store) token(service string) (tok string, gen int64) {
	gen = s.generation(service)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(service))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(gen, 10)))
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(m.Sum(nil)), gen
}

// feedSig is the secret part of a route's feed link. A feed reader can't send
// a token, so the link carries one; it is made from the route, and so lasts
// as long as the route does.
func (s *store) feedSig(route string) string {
	m := hmac.New(sha256.New, s.feedKey)
	m.Write([]byte("feed\x00"))
	m.Write([]byte(route))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))[:22]
}

func (s *store) aliasesOf(service string) []alias {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []alias
	for _, a := range s.aliases {
		if a.Service == service {
			out = append(out, a)
		}
	}
	return out
}

func (s *store) aliasFor(route string) (alias, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	a, ok := s.aliases[route]
	return a, ok
}

func (s *store) addAlias(a alias) error {
	if _, err := s.db.Exec(`INSERT INTO aliases (alias, service, note, created_at, created_by) VALUES (?, ?, ?, ?, ?)`,
		a.Alias, a.Service, a.Note, s.now().Unix(), a.CreatedBy); err != nil {
		return fmt.Errorf("save address: %w", err)
	}
	return s.reload()
}

func (s *store) removeAlias(route string) error {
	if _, err := s.db.Exec(`DELETE FROM aliases WHERE alias = ?`, route); err != nil {
		return fmt.Errorf("remove address: %w", err)
	}
	return s.reload()
}

func (s *store) canSend(service string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.senders[service]
}

func (s *store) setSend(service string, allowed bool, by string) error {
	var err error
	if allowed {
		_, err = s.db.Exec(`INSERT INTO senders (service, updated_at, updated_by) VALUES (?, ?, ?)
			ON CONFLICT(service) DO UPDATE SET updated_at = excluded.updated_at, updated_by = excluded.updated_by`,
			service, s.now().Unix(), by)
	} else {
		_, err = s.db.Exec(`DELETE FROM senders WHERE service = ?`, service)
	}
	if err != nil {
		return fmt.Errorf("save who may send: %w", err)
	}
	return s.reload()
}

const messageCols = `id, service, route, rcpt, from_name, from_addr, reply_to, subject, message_id, date, received, text, html, attachments`

// put keeps a message for its app, unless src was kept already. It reports
// whether it was new.
func (s *store) put(src string, m *Message) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages (src, service, route, rcpt, from_name, from_addr, reply_to,
		subject, message_id, date, received, text, html, attachments) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src, m.service, m.Route, m.To, m.From.Name, m.From.Address, m.ReplyTo, m.Subject, m.MessageID,
		m.Date.Unix(), m.Received.Unix(), m.Text, m.HTML, m.Attachments)
	if err != nil {
		return false, fmt.Errorf("keep message: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	m.ID, _ = res.LastInsertId()
	// Only the newest are kept, so an app that never reads its mail doesn't
	// grow mail.db for ever.
	if _, err := s.db.Exec(`DELETE FROM messages WHERE service = ? AND id NOT IN
		(SELECT id FROM messages WHERE service = ? ORDER BY id DESC LIMIT ?)`, m.service, m.service, keep); err != nil {
		return true, fmt.Errorf("trim %s's mail: %w", m.service, err)
	}
	return true, nil
}

func scanMessages(rows *sql.Rows) ([]Message, error) {
	defer func() { _ = rows.Close() }() // read-only
	var out []Message
	for rows.Next() {
		var m Message
		var date, received int64
		if err := rows.Scan(&m.ID, &m.service, &m.Route, &m.To, &m.From.Name, &m.From.Address, &m.ReplyTo,
			&m.Subject, &m.MessageID, &date, &received, &m.Text, &m.HTML, &m.Attachments); err != nil {
			return nil, err
		}
		m.Date, m.Received = time.Unix(date, 0).UTC(), time.Unix(received, 0).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// list is service's messages after id, oldest first.
func (s *store) list(service string, after int64, limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages WHERE service = ? AND id > ? ORDER BY id LIMIT ?`,
		service, after, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

// latest is route's newest messages, newest first, for its feed.
func (s *store) latest(route string, limit int) ([]Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages WHERE route = ? ORDER BY id DESC LIMIT ?`, route, limit)
	if err != nil {
		return nil, err
	}
	return scanMessages(rows)
}

var errNoMessage = errors.New("no such message")

func (s *store) get(service string, id int64) (Message, error) {
	rows, err := s.db.Query(`SELECT `+messageCols+` FROM messages WHERE service = ? AND id = ?`, service, id)
	if err != nil {
		return Message{}, err
	}
	ms, err := scanMessages(rows)
	if err != nil {
		return Message{}, err
	}
	if len(ms) == 0 {
		return Message{}, errNoMessage
	}
	return ms[0], nil
}

func (s *store) remove(service string, id int64) error {
	res, err := s.db.Exec(`DELETE FROM messages WHERE service = ? AND id = ?`, service, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNoMessage
	}
	return nil
}

// held is how many messages service has, and when the newest came.
func (s *store) held(service string) (int, time.Time) {
	var n int
	var last sql.NullInt64
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(received) FROM messages WHERE service = ?`, service).Scan(&n, &last); err != nil {
		return 0, time.Time{}
	}
	if !last.Valid {
		return n, time.Time{}
	}
	return n, time.Unix(last.Int64, 0)
}

// sentSince counts what service sent after t, for the daily limit.
func (s *store) sentSince(service string, t time.Time) int {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sent WHERE service = ? AND at > ?`, service, t.Unix()).Scan(&n); err != nil {
		return 0
	}
	return n
}

func (s *store) recordSent(service, route string, rcpts []string, subject, messageID string) error {
	_, err := s.db.Exec(`INSERT INTO sent (service, route, at, rcpts, subject, message_id) VALUES (?, ?, ?, ?, ?, ?)`,
		service, route, s.now().Unix(), strings.Join(rcpts, ", "), subject, messageID)
	return err
}

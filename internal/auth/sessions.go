package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// PortalSessionTTL is how long a session lasts without being used.
const PortalSessionTTL = 30 * 24 * time.Hour

// RefreshAfter is how stale a session's expiry has to be before a touch
// slides it forward. Sliding on every request would mean a database write per
// request for no practical gain.
const RefreshAfter = 24 * time.Hour

// maxUserAgent bounds what a client can make the store keep.
const maxUserAgent = 256

// ErrNoSuchSession covers a token that was never issued, has expired, was
// revoked, or belongs to a disabled user. Callers treat them all the same:
// send the browser to log in.
var ErrNoSuchSession = errors.New("no such session")

// Session is one live login.
type Session struct {
	Username  string
	App       string // empty for the portal session
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession starts a session and returns its token, which is never
// stored. A portal session has no app and no parent. An app session names the
// app and the portal session it came from; ending the portal session ends it.
func (s *Store) CreateSession(username, app, parentToken, userAgent string) (string, error) {
	return s.CreateSessionFrom(username, app, parentToken, userAgent, "")
}

// maxIP bounds the address kept with a session.
const maxIP = 64

// CreateSessionFrom is CreateSession, also keeping the client address the
// sign-in came from, so the user can tell their sign-ins apart by something
// other than what their browser claims to be.
func (s *Store) CreateSessionFrom(username, app, parentToken, userAgent, ip string) (string, error) {
	if len(ip) > maxIP {
		ip = ip[:maxIP]
	}
	if (app == "") != (parentToken == "") {
		return "", errors.New("an app session needs a parent session, and a portal session can't have one")
	}
	if len(userAgent) > maxUserAgent {
		// Back up to a character boundary, so a cut never stores half a
		// UTF-8 sequence.
		cut := maxUserAgent
		for cut > 0 && !utf8.RuneStart(userAgent[cut]) {
			cut--
		}
		userAgent = userAgent[:cut]
	}
	tok, err := NewToken()
	if err != nil {
		return "", err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer rollback(tx)

	var (
		id       int64
		disabled bool
	)
	err = tx.QueryRow(`SELECT id, disabled FROM users WHERE username = ?`, username).Scan(&id, &disabled)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoSuchUser, username)
	}
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", username, err)
	}
	if disabled {
		return "", fmt.Errorf("%s is disabled", username)
	}

	now := s.now()
	parentHash := ""
	if parentToken != "" {
		parentHash = HashToken(parentToken)
		var parentUser int64
		err := tx.QueryRow(`SELECT user_id FROM sessions WHERE token_hash = ? AND app = '' AND expires_at > ?`,
			parentHash, now.Unix()).Scan(&parentUser)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && parentUser != id) {
			return "", fmt.Errorf("parent %w", ErrNoSuchSession)
		}
		if err != nil {
			return "", fmt.Errorf("look up parent session: %w", err)
		}
	}

	if _, err := tx.Exec(`
INSERT INTO sessions (token_hash, user_id, parent_hash, app, created_at, expires_at, last_seen_at, user_agent, ip)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		HashToken(tok), id, parentHash, app, now.Unix(), now.Add(PortalSessionTTL).Unix(), now.Unix(), userAgent, ip); err != nil {
		return "", fmt.Errorf("create session: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return tok, nil
}

// LookupSession returns the session a token names and the user it belongs to.
// An app session is only as alive as its parent.
func (s *Store) LookupSession(token string) (Session, User, error) {
	now := s.now().Unix()
	var (
		sess             Session
		username         string
		parentHash       string
		created, expires int64
		disabled         bool
	)
	err := s.db.QueryRow(`
SELECT u.username, u.disabled, s.app, s.parent_hash, s.created_at, s.expires_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.token_hash = ? AND s.expires_at > ?`, HashToken(token), now).
		Scan(&username, &disabled, &sess.App, &parentHash, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && disabled) {
		return Session{}, User{}, ErrNoSuchSession
	}
	if err != nil {
		return Session{}, User{}, fmt.Errorf("look up session: %w", err)
	}
	if parentHash != "" {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ? AND expires_at > ?`,
			parentHash, now).Scan(&n); err != nil {
			return Session{}, User{}, fmt.Errorf("look up parent session: %w", err)
		}
		if n == 0 {
			return Session{}, User{}, ErrNoSuchSession
		}
	}

	u, err := s.GetUser(username)
	if err != nil {
		return Session{}, User{}, err
	}
	sess.Username = username
	sess.CreatedAt = fromUnix(created)
	sess.ExpiresAt = fromUnix(expires)
	return sess, u, nil
}

// TouchSession records that a session was used. Its expiry slides forward to
// a full PortalSessionTTL, but only once it was last set more than
// RefreshAfter ago.
func (s *Store) TouchSession(token string) error {
	now := s.now()
	h := HashToken(token)
	var expires int64
	err := s.db.QueryRow(`SELECT expires_at FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		h, now.Unix()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNoSuchSession
	}
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	setAt := time.Unix(expires, 0).Add(-PortalSessionTTL)
	if now.Sub(setAt) < RefreshAfter {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE sessions SET expires_at = ?, last_seen_at = ? WHERE token_hash = ?`,
		now.Add(PortalSessionTTL).Unix(), now.Unix(), h); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RevokeSession ends a session and every app session derived from it.
// Revoking a session that doesn't exist is not an error: logging out twice
// should just work.
func (s *Store) RevokeSession(token string) error {
	h := HashToken(token)
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ? OR parent_hash = ?`, h, h); err != nil {
		return fmt.Errorf("revoke session: %w", err)
	}
	return nil
}

// RevokeUserSessions ends every session and access token a user has and
// reports how many sessions.
func (s *Store) RevokeUserSessions(username string) (int, error) {
	sessions, _, err := s.RevokeUserAccess(username)
	return sessions, err
}

// PruneExpired deletes expired sessions, and app sessions whose parent is
// gone, and reports how many rows went.
func (s *Store) PruneExpired() (int, error) {
	res, err := s.db.Exec(`
DELETE FROM sessions
WHERE expires_at <= ?
   OR (parent_hash != '' AND parent_hash NOT IN (SELECT token_hash FROM sessions WHERE expires_at > ?))`,
		s.now().Unix(), s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("prune sessions: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// parentOf returns the hash of the portal session a token's session hangs
// from: its parent for an app session, its own hash for a portal session.
func (s *Store) parentOf(token string) (string, error) {
	h := HashToken(token)
	var parent string
	err := s.db.QueryRow(`SELECT parent_hash FROM sessions WHERE token_hash = ? AND expires_at > ?`,
		h, s.now().Unix()).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoSuchSession
	}
	if err != nil {
		return "", fmt.Errorf("look up session: %w", err)
	}
	if parent == "" {
		return h, nil
	}
	return parent, nil
}

// TouchFamily records that a session was used, sliding it and, for an app
// session, its portal session too, each on TouchSession's terms. Using any
// app counts as using the login, so someone who only ever opens one app
// isn't sent back to the login page when the untouched portal session runs
// out.
func (s *Store) TouchFamily(token string) error {
	parent, err := s.parentOf(token)
	if err != nil {
		return err
	}
	now := s.now()
	slide := now.Add(PortalSessionTTL).Unix()
	// Only rows last slid more than RefreshAfter ago, as in TouchSession.
	stale := now.Add(PortalSessionTTL - RefreshAfter).Unix()
	if _, err := s.db.Exec(`
UPDATE sessions SET expires_at = ?, last_seen_at = ?
WHERE token_hash IN (?, ?) AND expires_at > ? AND expires_at <= ?`,
		slide, now.Unix(), HashToken(token), parent, now.Unix(), stale); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// RevokeFamily ends the login a session belongs to: its portal session and
// every app session under it. Logging out from any app host means this. A
// token that names no live session is not an error.
func (s *Store) RevokeFamily(token string) error {
	parent, err := s.parentOf(token)
	if errors.Is(err, ErrNoSuchSession) {
		return s.RevokeSession(token)
	}
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash = ? OR parent_hash = ? OR token_hash = ?`,
		parent, parent, HashToken(token)); err != nil {
		return fmt.Errorf("revoke sessions: %w", err)
	}
	return nil
}

// SignIn is one login: a portal session and the app sessions it handed
// out, which end with it.
type SignIn struct {
	// ID names the sign-in to the account page. It is the portal session's
	// stored hash, which can't be turned back into the token.
	ID        string
	UserAgent string
	// IP is the address the sign-in came from, "" for one from before
	// addresses were kept.
	IP        string
	CreatedAt time.Time
	// LastSeen is the latest use of the portal session or any app under it.
	LastSeen time.Time
	// Apps are the apps opened in it, by name, sorted.
	Apps []string
}

// SignIns lists a user's live sign-ins, most recently used first.
func (s *Store) SignIns(username string) ([]SignIn, error) {
	if _, err := mustUserID(s.db, username); err != nil {
		return nil, err
	}
	now := s.now().Unix()
	rows, err := s.db.Query(`
SELECT s.token_hash, s.parent_hash, s.app, s.user_agent, s.ip, s.created_at, s.last_seen_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE u.username = ? AND s.expires_at > ?`, username, now)
	if err != nil {
		return nil, fmt.Errorf("list sessions for %s: %w", username, err)
	}
	defer rows.Close()

	byID := map[string]*SignIn{}
	type appRow struct {
		parent, app string
		seen        int64
	}
	var apps []appRow
	for rows.Next() {
		var hash, parent, app, agent, ip string
		var created, seen int64
		if err := rows.Scan(&hash, &parent, &app, &agent, &ip, &created, &seen); err != nil {
			return nil, fmt.Errorf("list sessions for %s: %w", username, err)
		}
		if parent != "" {
			apps = append(apps, appRow{parent, app, seen})
			continue
		}
		byID[hash] = &SignIn{ID: hash, UserAgent: agent, IP: ip, CreatedAt: fromUnix(created), LastSeen: fromUnix(seen)}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, a := range apps {
		si, ok := byID[a.parent]
		if !ok {
			continue // its portal session has gone; it's dead and awaiting a prune
		}
		if !slices.Contains(si.Apps, a.app) {
			si.Apps = append(si.Apps, a.app)
		}
		if t := fromUnix(a.seen); t.After(si.LastSeen) {
			si.LastSeen = t
		}
	}
	out := make([]SignIn, 0, len(byID))
	for _, si := range byID {
		slices.Sort(si.Apps)
		out = append(out, *si)
	}
	slices.SortFunc(out, func(a, b SignIn) int {
		if c := b.LastSeen.Compare(a.LastSeen); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// EndSignIn ends one of a user's sign-ins, by its ID, with every app
// session under it. A sign-in that is already over, or isn't theirs, is
// ErrNoSuchSession, so one user can't end another's by guessing.
func (s *Store) EndSignIn(username, id string) error {
	res, err := s.db.Exec(`
DELETE FROM sessions
WHERE (token_hash = ? OR parent_hash = ?)
  AND user_id = (SELECT id FROM users WHERE username = ?)
  AND EXISTS (SELECT 1 FROM sessions p JOIN users u ON u.id = p.user_id
              WHERE p.token_hash = ? AND p.parent_hash = '' AND u.username = ?)`,
		id, id, username, id, username)
	if err != nil {
		return fmt.Errorf("end sign-in: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNoSuchSession
	}
	return nil
}

// EndOtherSignIns ends every sign-in a user has except keep, the one they
// are using, and reports how many ended.
func (s *Store) EndOtherSignIns(username, keep string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollback(tx)
	id, err := mustUserID(tx, username)
	if err != nil {
		return 0, err
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND parent_hash = '' AND token_hash != ? AND expires_at > ?`,
		id, keep, s.now().Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count sign-ins for %s: %w", username, err)
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ? AND token_hash != ? AND parent_hash != ?`,
		id, keep, keep); err != nil {
		return 0, fmt.Errorf("end other sign-ins for %s: %w", username, err)
	}
	return n, tx.Commit()
}

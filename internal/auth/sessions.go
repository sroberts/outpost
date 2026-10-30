package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
INSERT INTO sessions (token_hash, user_id, parent_hash, app, created_at, expires_at, last_seen_at, user_agent)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		HashToken(tok), id, parentHash, app, now.Unix(), now.Add(PortalSessionTTL).Unix(), now.Unix(), userAgent); err != nil {
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

// RevokeUserSessions ends every session a user has and reports how many.
func (s *Store) RevokeUserSessions(username string) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return 0, err
	}
	// Count only live sessions: expired rows awaiting a prune were already
	// over, and reporting them as ended would overstate what this did.
	var live int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`,
		id, s.now().Unix()).Scan(&live); err != nil {
		return 0, fmt.Errorf("count sessions for %s: %w", username, err)
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return 0, fmt.Errorf("end sessions for %s: %w", username, err)
	}
	return live, tx.Commit()
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

// SignIn is one login as its owner sees it: a portal session and the app
// sessions it handed out.
type SignIn struct {
	// ID is the portal session's token hash. It names the sign-in in a form
	// and can't be turned back into the token, so it is no use as a credential.
	ID         string
	UserAgent  string
	Apps       []string // the apps signed in to, sorted, each once
	CreatedAt  time.Time
	LastSeenAt time.Time // moves only when a session slides, so up to a day behind
}

// SignIns lists a user's live logins, most recently used first.
func (s *Store) SignIns(username string) ([]SignIn, error) {
	rows, err := s.db.Query(`
SELECT s.token_hash, s.parent_hash, s.app, s.user_agent, s.created_at, s.last_seen_at
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE u.username = ? AND s.expires_at > ?`, username, s.now().Unix())
	if err != nil {
		return nil, fmt.Errorf("list sessions for %s: %w", username, err)
	}
	defer rows.Close()

	byID := map[string]*SignIn{}
	apps := map[string]map[string]bool{}
	for rows.Next() {
		var (
			hash, parent, app, agent string
			created, seen            int64
		)
		if err := rows.Scan(&hash, &parent, &app, &agent, &created, &seen); err != nil {
			return nil, fmt.Errorf("list sessions for %s: %w", username, err)
		}
		if parent == "" {
			byID[hash] = &SignIn{ID: hash, UserAgent: agent, CreatedAt: fromUnix(created), LastSeenAt: fromUnix(seen)}
			continue
		}
		if apps[parent] == nil {
			apps[parent] = map[string]bool{}
		}
		apps[parent][app] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sessions for %s: %w", username, err)
	}

	out := make([]SignIn, 0, len(byID))
	for id, in := range byID {
		for app := range apps[id] {
			in.Apps = append(in.Apps, app)
		}
		sort.Strings(in.Apps)
		out = append(out, *in)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeenAt.Equal(out[j].LastSeenAt) {
			return out[i].LastSeenAt.After(out[j].LastSeenAt)
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// EndSignIn ends one of a user's logins and every app session under it, and
// reports whether there was one to end. The username is part of the match, so
// nobody can end someone else's login by naming its ID.
func (s *Store) EndSignIn(username, id string) (bool, error) {
	// An empty ID would match parent_hash on every portal session below.
	if id == "" {
		return false, nil
	}
	res, err := s.db.Exec(`
DELETE FROM sessions
WHERE user_id = (SELECT id FROM users WHERE username = ?)
  AND ((token_hash = ? AND parent_hash = '') OR parent_hash = ?)`, username, id, id)
	if err != nil {
		return false, fmt.Errorf("end session for %s: %w", username, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

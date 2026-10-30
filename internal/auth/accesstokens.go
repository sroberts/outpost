package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// AccessTokenPrefix starts every personal access token. It is how the gateway
// tells one of its own from a bearer token that belongs to the app behind it.
const AccessTokenPrefix = "opat_"

// AccessTokenTouchEvery is how often a token's last-used time moves. Scripts
// can call far more often than anyone needs that time to be accurate, and each
// move is a database write.
const AccessTokenTouchEvery = time.Hour

// ErrNoSuchAccessToken covers a token that was never issued, has expired, was
// revoked, or belongs to a disabled user. Callers treat them all the same, and
// a client isn't told which.
var ErrNoSuchAccessToken = errors.New("no such access token")

// AccessToken is one personal access token as callers see it. It carries no
// hash and no secret.
type AccessToken struct {
	ID         int64
	Username   string
	Name       string
	Apps       []string // sorted; empty means every app the user may reach
	CreatedAt  time.Time
	ExpiresAt  time.Time // zero means never
	LastUsedAt time.Time // zero means never; up to AccessTokenTouchEvery behind
}

// Reaches reports whether the token's scope covers an app. It says nothing
// about whether the user may open the app, which is decided at each request.
func (t AccessToken) Reaches(app string) bool {
	if len(t.Apps) == 0 {
		return true
	}
	for _, a := range t.Apps {
		if a == app {
			return true
		}
	}
	return false
}

// Expired reports whether the token had run out by now.
func (t AccessToken) Expired(now time.Time) bool {
	return !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(now)
}

// validTokenApp checks one entry of a token's scope. An empty name has to be
// refused, not dropped: dropping the only one would leave an empty list, which
// means every app, so a typo in a script would widen the token.
func validTokenApp(app string) error {
	if app == "" {
		return errors.New("an app name in a token's scope can't be empty")
	}
	if strings.ContainsAny(app, " ,\t") {
		return fmt.Errorf("invalid app name %q: give each app its own --app", app)
	}
	return validText("app name", app)
}

// CreateAccessToken issues a token for a user and returns its secret, which is
// never stored and can't be recovered. No apps means every app the user may
// reach. A ttl of zero means it doesn't expire.
func (s *Store) CreateAccessToken(username, name string, apps []string, ttl time.Duration) (string, AccessToken, error) {
	if err := validText("token name", name); err != nil {
		return "", AccessToken{}, err
	}
	for _, app := range apps {
		if err := validTokenApp(app); err != nil {
			return "", AccessToken{}, err
		}
	}
	if ttl < 0 {
		return "", AccessToken{}, errors.New("a token's expiry has to be in the future")
	}
	tok, err := NewToken()
	if err != nil {
		return "", AccessToken{}, err
	}
	secret := AccessTokenPrefix + tok

	tx, err := s.db.Begin()
	if err != nil {
		return "", AccessToken{}, err
	}
	defer rollback(tx)

	r, err := s.getUser(tx, username)
	if err != nil {
		return "", AccessToken{}, err
	}
	// Disabling revokes a user's tokens, so one issued afterwards would be the
	// only thing outliving it.
	if r.Disabled {
		return "", AccessToken{}, fmt.Errorf("%s is disabled", username)
	}

	now := s.now()
	var expires int64
	if ttl > 0 {
		expires = now.Add(ttl).Unix()
	}
	res, err := tx.Exec(`
INSERT INTO access_tokens (token_hash, user_id, name, created_at, expires_at)
VALUES (?, ?, ?, ?, ?)`, HashToken(secret), r.id, name, now.Unix(), expires)
	if err != nil {
		return "", AccessToken{}, fmt.Errorf("create access token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", AccessToken{}, err
	}
	for _, app := range apps {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO access_token_apps (token_id, app) VALUES (?, ?)`, id, app); err != nil {
			return "", AccessToken{}, fmt.Errorf("scope access token: %w", err)
		}
	}
	t := AccessToken{ID: id, Username: username, Name: name, CreatedAt: fromUnix(now.Unix()), ExpiresAt: fromUnix(expires)}
	if t.Apps, err = tokenApps(tx, id); err != nil {
		return "", AccessToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", AccessToken{}, err
	}
	return secret, t, nil
}

// LookupAccessToken returns the token a secret names and the user it belongs
// to, as they are now: the user's groups and admin bit are never a copy from
// when the token was made.
func (s *Store) LookupAccessToken(secret string) (AccessToken, User, error) {
	var (
		t                      AccessToken
		created, expires, used int64
		disabled               bool
	)
	err := s.db.QueryRow(`
SELECT t.id, u.username, u.disabled, t.name, t.created_at, t.expires_at, t.last_used_at
FROM access_tokens t JOIN users u ON u.id = t.user_id
WHERE t.token_hash = ? AND (t.expires_at = 0 OR t.expires_at > ?)`, HashToken(secret), s.now().Unix()).
		Scan(&t.ID, &t.Username, &disabled, &t.Name, &created, &expires, &used)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && disabled) {
		return AccessToken{}, User{}, ErrNoSuchAccessToken
	}
	if err != nil {
		return AccessToken{}, User{}, fmt.Errorf("look up access token: %w", err)
	}
	t.CreatedAt, t.ExpiresAt, t.LastUsedAt = fromUnix(created), fromUnix(expires), fromUnix(used)
	if t.Apps, err = tokenApps(s.db, t.ID); err != nil {
		return AccessToken{}, User{}, err
	}
	// The user is read again apart from the token, so a disable or removal
	// that lands in between is checked here too, not served as a 500 or as a
	// request from a user who has just been shut out.
	u, err := s.GetUser(t.Username)
	if errors.Is(err, ErrNoSuchUser) || (err == nil && u.Disabled) {
		return AccessToken{}, User{}, ErrNoSuchAccessToken
	}
	if err != nil {
		return AccessToken{}, User{}, err
	}
	return t, u, nil
}

// TouchAccessToken records that a token was used, if the time recorded is due
// to move, so a busy script isn't a database write per request.
func (s *Store) TouchAccessToken(t AccessToken) error {
	now := s.now()
	if now.Sub(t.LastUsedAt) < AccessTokenTouchEvery {
		return nil
	}
	if _, err := s.db.Exec(`UPDATE access_tokens SET last_used_at = ? WHERE id = ?`, now.Unix(), t.ID); err != nil {
		return fmt.Errorf("touch access token: %w", err)
	}
	return nil
}

// ListAccessTokens returns one user's tokens, or everyone's when username is
// empty, oldest first. Expired tokens stay listed until revoked, so whoever is
// working out why a script stopped can see the reason.
func (s *Store) ListAccessTokens(username string) ([]AccessToken, error) {
	const cols = `
SELECT t.id, u.username, t.name, t.created_at, t.expires_at, t.last_used_at
FROM access_tokens t JOIN users u ON u.id = t.user_id`
	var (
		rows *sql.Rows
		err  error
	)
	if username == "" {
		rows, err = s.db.Query(cols + ` ORDER BY t.id`)
	} else {
		if _, err := mustUserID(s.db, username); err != nil {
			return nil, err
		}
		rows, err = s.db.Query(cols+` WHERE u.username = ? ORDER BY t.id`, username)
	}
	if err != nil {
		return nil, fmt.Errorf("list access tokens: %w", err)
	}
	var out []AccessToken
	for rows.Next() {
		var (
			t                      AccessToken
			created, expires, used int64
		)
		if err := rows.Scan(&t.ID, &t.Username, &t.Name, &created, &expires, &used); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list access tokens: %w", err)
		}
		t.CreatedAt, t.ExpiresAt, t.LastUsedAt = fromUnix(created), fromUnix(expires), fromUnix(used)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("list access tokens: %w", err)
	}
	// The store has one connection, so the cursor must be closed before the
	// per-token queries below can run.
	rows.Close()

	for i := range out {
		if out[i].Apps, err = tokenApps(s.db, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// RevokeAccessToken ends one token and returns what it was.
func (s *Store) RevokeAccessToken(id int64) (AccessToken, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return AccessToken{}, err
	}
	defer rollback(tx)

	var t AccessToken
	err = tx.QueryRow(`
SELECT t.id, u.username, t.name FROM access_tokens t JOIN users u ON u.id = t.user_id
WHERE t.id = ?`, id).Scan(&t.ID, &t.Username, &t.Name)
	if errors.Is(err, sql.ErrNoRows) {
		return AccessToken{}, fmt.Errorf("%w: %d", ErrNoSuchAccessToken, id)
	}
	if err != nil {
		return AccessToken{}, fmt.Errorf("look up access token %d: %w", id, err)
	}
	if _, err := tx.Exec(`DELETE FROM access_tokens WHERE id = ?`, id); err != nil {
		return AccessToken{}, fmt.Errorf("revoke access token %d: %w", id, err)
	}
	return t, tx.Commit()
}

// CountAccessTokens reports how many unexpired tokens a user has, so whatever
// is about to end them can say so.
func (s *Store) CountAccessTokens(username string) (int, error) {
	var n int
	err := s.db.QueryRow(`
SELECT COUNT(*) FROM access_tokens t JOIN users u ON u.id = t.user_id
WHERE u.username = ? AND (t.expires_at = 0 OR t.expires_at > ?)`, username, s.now().Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count access tokens for %s: %w", username, err)
	}
	return n, nil
}

// RevokeUserAccess ends every session and access token a user has and reports
// how many of each. Counts are of live ones only: expired rows awaiting a prune
// were already over, and reporting them as ended would overstate what this did.
func (s *Store) RevokeUserAccess(username string) (sessions, tokens int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return 0, 0, err
	}
	now := s.now().Unix()
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`,
		id, now).Scan(&sessions); err != nil {
		return 0, 0, fmt.Errorf("count sessions for %s: %w", username, err)
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM access_tokens WHERE user_id = ? AND (expires_at = 0 OR expires_at > ?)`,
		id, now).Scan(&tokens); err != nil {
		return 0, 0, fmt.Errorf("count access tokens for %s: %w", username, err)
	}
	if err := endUserAccess(tx, id, username); err != nil {
		return 0, 0, err
	}
	return sessions, tokens, tx.Commit()
}

// endUserAccess deletes everything that lets a user in without a password.
// Every path that shuts someone out goes through here, so a token can't outlive
// a browser session on any of them; removing a user gets the same from the
// schema's cascades.
func endUserAccess(q querier, id int64, username string) error {
	if _, err := q.Exec(`DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("end sessions for %s: %w", username, err)
	}
	if _, err := q.Exec(`DELETE FROM access_tokens WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("revoke access tokens for %s: %w", username, err)
	}
	return nil
}

func tokenApps(q querier, id int64) ([]string, error) {
	rows, err := q.Query(`SELECT app FROM access_token_apps WHERE token_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("load access token scope: %w", err)
	}
	defer rows.Close()
	var apps []string
	for rows.Next() {
		var app string
		if err := rows.Scan(&app); err != nil {
			return nil, fmt.Errorf("load access token scope: %w", err)
		}
		apps = append(apps, app)
	}
	sort.Strings(apps)
	return apps, rows.Err()
}

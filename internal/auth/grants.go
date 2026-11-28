package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// RefreshTokenPrefix starts every OAuth refresh token. It differs from
// AccessTokenPrefix so a refresh token is never taken for an access token: the
// gateway only claims bearer tokens that start with opat_.
const RefreshTokenPrefix = "oprt_"

// GrantAccessTTL is how long an access token issued under a grant lasts. The
// client renews it with its refresh token, so it can be short.
const GrantAccessTTL = time.Hour

// GrantIdleTTL is how long a grant lasts without a refresh. Each refresh
// extends it, so a client in use stays connected and one that has gone away
// lapses.
const GrantIdleTTL = 90 * 24 * time.Hour

// ErrInvalidGrant covers a refresh token that was never issued, has been
// replaced, belongs to a lapsed or revoked grant or a disabled user, or was
// presented by another client or for another app, and a grant CreateGrant
// won't make for what it was asked. The token endpoint answers them all with
// invalid_grant; any other error is the server's, not the client's.
var ErrInvalidGrant = errors.New("invalid grant")

// ErrRefreshReused is a refresh token that was already exchanged, presented
// again. Rotation means only one party can hold the current token, so a second
// use is a copy: the grant has been revoked, and RefreshGrant returns it so
// the caller can say so.
var ErrRefreshReused = errors.New("refresh token reused; the grant was revoked")

// ErrNoSuchGrant is a grant id that doesn't exist, or no longer does.
var ErrNoSuchGrant = errors.New("no such connected client")

// Grant is one OAuth client's access to one app's MCP endpoint, as one user.
// It carries no hash and no secret.
type Grant struct {
	ID         int64
	Username   string
	App        string
	ClientID   string // the URL of the client's metadata document
	ClientName string
	CreatedAt  time.Time
	// RefreshedAt is when the client last renewed its access token; zero
	// until it first does.
	RefreshedAt time.Time
	// ExpiresAt is when the grant lapses unless it is refreshed first.
	ExpiresAt time.Time
}

// Lapsed reports whether the grant has gone unused too long to be renewed.
func (g Grant) Lapsed(now time.Time) bool { return !g.ExpiresAt.After(now) }

// clientNameMax keeps a client's self-chosen name short enough for a list
// row and an activity line.
const clientNameMax = 80

// CleanClientName makes a name from a client's metadata document fit to show:
// control characters out, trimmed, and cut to clientNameMax runes. The name is
// the client's own claim, so callers show the client's host beside it.
func CleanClientName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if r := []rune(name); len(r) > clientNameMax {
		name = string(r[:clientNameMax])
	}
	return name
}

// CreateGrant records that username let a client use app's MCP endpoint, and
// returns the grant with its first access token and refresh token. The access
// token is an internet token scoped to app alone, so the gateway checks it
// exactly as it checks one made with `outpost token create --internet`.
func (s *Store) CreateGrant(username, app, clientID, clientName string) (access, refresh string, g Grant, err error) {
	if err := validTokenApp(app); err != nil {
		return "", "", Grant{}, fmt.Errorf("%w: %w", ErrInvalidGrant, err)
	}
	if clientID == "" || len(clientID) > 2048 || validText("client id", clientID) != nil {
		return "", "", Grant{}, fmt.Errorf("%w: a client id is required, as a URL of at most 2048 bytes", ErrInvalidGrant)
	}
	clientName = CleanClientName(clientName)
	if refresh, err = newRefreshToken(); err != nil {
		return "", "", Grant{}, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return "", "", Grant{}, err
	}
	defer rollback(tx)

	r, err := s.getUser(tx, username)
	if errors.Is(err, ErrNoSuchUser) {
		return "", "", Grant{}, fmt.Errorf("%w: %w", ErrInvalidGrant, err)
	}
	if err != nil {
		return "", "", Grant{}, err
	}
	// As with tokens: disabling ends a user's grants, so one made afterwards
	// would be the only thing outliving it.
	if r.Disabled {
		return "", "", Grant{}, fmt.Errorf("%w: %s is disabled", ErrInvalidGrant, username)
	}
	now := s.now()
	// Grants lapse rather than being swept, so making one is when the lapsed
	// ones go.
	if err := deleteLapsedGrants(tx, now); err != nil {
		return "", "", Grant{}, err
	}
	res, err := tx.Exec(`
INSERT INTO oauth_grants (user_id, app, client_id, client_name, refresh_hash, created_at, expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`, r.id, app, clientID, clientName, HashToken(refresh), now.Unix(), now.Add(GrantIdleTTL).Unix())
	if err != nil {
		return "", "", Grant{}, fmt.Errorf("create grant: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", "", Grant{}, err
	}
	if access, err = insertGrantToken(tx, r.id, id, app, clientName, now); err != nil {
		return "", "", Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", Grant{}, err
	}
	g = Grant{
		ID: id, Username: username, App: app, ClientID: clientID, ClientName: clientName,
		CreatedAt: fromUnix(now.Unix()), ExpiresAt: fromUnix(now.Add(GrantIdleTTL).Unix()),
	}
	return access, refresh, g, nil
}

// RefreshGrant exchanges a refresh token for a new access token and a new
// refresh token, and the old one stops working. clientID and app must be the
// grant's. A refresh token that was already exchanged revokes the grant and
// returns ErrRefreshReused with it; anything else wrong is ErrInvalidGrant.
func (s *Store) RefreshGrant(refresh, clientID, app string) (access, next string, g Grant, err error) {
	if !strings.HasPrefix(refresh, RefreshTokenPrefix) {
		return "", "", Grant{}, ErrInvalidGrant
	}
	if next, err = newRefreshToken(); err != nil {
		return "", "", Grant{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return "", "", Grant{}, err
	}
	defer rollback(tx)

	now := s.now()
	h := HashToken(refresh)
	var (
		userID   int64
		disabled bool
	)
	g, userID, disabled, err = grantBy(tx, `g.refresh_hash = ?`, h)
	if errors.Is(err, sql.ErrNoRows) {
		// Not current: it may be the one before, presented again.
		g, _, _, err = grantBy(tx, `g.previous_hash = ?`, h)
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", Grant{}, ErrInvalidGrant
		}
		if err != nil {
			return "", "", Grant{}, err
		}
		if err := deleteGrant(tx, g.ID); err != nil {
			return "", "", Grant{}, err
		}
		if err := tx.Commit(); err != nil {
			return "", "", Grant{}, err
		}
		return "", "", g, ErrRefreshReused
	}
	if err != nil {
		return "", "", Grant{}, err
	}
	// Presented by another client, or for another app: refused, but the grant
	// is left alone, since the token itself wasn't replayed.
	if g.ClientID != clientID || g.App != app || disabled || g.Lapsed(now) {
		return "", "", Grant{}, ErrInvalidGrant
	}
	expires := now.Add(GrantIdleTTL)
	if _, err := tx.Exec(`
UPDATE oauth_grants SET previous_hash = refresh_hash, refresh_hash = ?, refreshed_at = ?, expires_at = ?
WHERE id = ?`, HashToken(next), now.Unix(), expires.Unix(), g.ID); err != nil {
		return "", "", Grant{}, fmt.Errorf("rotate refresh token: %w", err)
	}
	// The grant's spent access tokens go as it renews, so they don't pile up.
	if _, err := tx.Exec(`DELETE FROM access_tokens WHERE grant_id = ? AND expires_at <= ?`, g.ID, now.Unix()); err != nil {
		return "", "", Grant{}, fmt.Errorf("prune grant tokens: %w", err)
	}
	if access, err = insertGrantToken(tx, userID, g.ID, g.App, g.ClientName, now); err != nil {
		return "", "", Grant{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", "", Grant{}, err
	}
	g.RefreshedAt, g.ExpiresAt = fromUnix(now.Unix()), fromUnix(expires.Unix())
	return access, next, g, nil
}

// ListGrants returns one user's grants, or everyone's when username is
// empty, oldest first. Lapsed grants stay listed until revoked or until the
// next grant is made, so whoever is working out why a client stopped can see
// why.
func (s *Store) ListGrants(username string) ([]Grant, error) {
	where, args := "1 = 1", []any{}
	if username != "" {
		if _, err := mustUserID(s.db, username); err != nil {
			return nil, err
		}
		where, args = "u.username = ?", []any{username}
	}
	rows, err := s.db.Query(grantSelect+` WHERE `+where+` ORDER BY g.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list connected clients: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		g, _, _, err := scanGrant(rows)
		if err != nil {
			return nil, fmt.Errorf("list connected clients: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant ends a grant and every access token issued under it, and
// returns what it was.
func (s *Store) RevokeGrant(id int64) (Grant, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Grant{}, err
	}
	defer rollback(tx)
	g, _, _, err := grantBy(tx, `g.id = ?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return Grant{}, fmt.Errorf("%w: %d", ErrNoSuchGrant, id)
	}
	if err != nil {
		return Grant{}, err
	}
	if err := deleteGrant(tx, id); err != nil {
		return Grant{}, err
	}
	return g, tx.Commit()
}

const grantSelect = `
SELECT g.id, u.username, g.app, g.client_id, g.client_name, g.created_at, g.refreshed_at, g.expires_at, g.user_id, u.disabled
FROM oauth_grants g JOIN users u ON u.id = g.user_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanGrant(row rowScanner) (Grant, int64, bool, error) {
	var (
		g                           Grant
		created, refreshed, expires int64
		userID                      int64
		disabled                    bool
	)
	if err := row.Scan(&g.ID, &g.Username, &g.App, &g.ClientID, &g.ClientName, &created, &refreshed, &expires, &userID, &disabled); err != nil {
		return Grant{}, 0, false, err
	}
	g.CreatedAt, g.RefreshedAt, g.ExpiresAt = fromUnix(created), fromUnix(refreshed), fromUnix(expires)
	return g, userID, disabled, nil
}

func grantBy(q querier, cond string, arg any) (Grant, int64, bool, error) {
	return scanGrant(q.QueryRow(grantSelect+` WHERE `+cond, arg))
}

// deleteGrant removes a grant and the access tokens issued under it.
func deleteGrant(q querier, id int64) error {
	if _, err := q.Exec(`DELETE FROM access_tokens WHERE grant_id = ?`, id); err != nil {
		return fmt.Errorf("revoke grant %d's tokens: %w", id, err)
	}
	if _, err := q.Exec(`DELETE FROM oauth_grants WHERE id = ?`, id); err != nil {
		return fmt.Errorf("revoke grant %d: %w", id, err)
	}
	return nil
}

func deleteLapsedGrants(q querier, now time.Time) error {
	if _, err := q.Exec(`DELETE FROM access_tokens WHERE grant_id IN (SELECT id FROM oauth_grants WHERE expires_at <= ?)`, now.Unix()); err != nil {
		return fmt.Errorf("prune lapsed grants' tokens: %w", err)
	}
	if _, err := q.Exec(`DELETE FROM oauth_grants WHERE expires_at <= ?`, now.Unix()); err != nil {
		return fmt.Errorf("prune lapsed grants: %w", err)
	}
	return nil
}

// insertGrantToken issues an access token under a grant: an internet token
// for app alone, lasting GrantAccessTTL.
func insertGrantToken(q querier, userID, grantID int64, app, clientName string, now time.Time) (string, error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	secret := AccessTokenPrefix + tok
	name := clientName
	if name == "" {
		name = "OAuth client"
	}
	res, err := q.Exec(`
INSERT INTO access_tokens (token_hash, user_id, name, created_at, expires_at, internet, grant_id)
VALUES (?, ?, ?, ?, ?, 1, ?)`, HashToken(secret), userID, name, now.Unix(), now.Add(GrantAccessTTL).Unix(), grantID)
	if err != nil {
		return "", fmt.Errorf("issue access token: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return "", err
	}
	if _, err := q.Exec(`INSERT INTO access_token_apps (token_id, app) VALUES (?, ?)`, id, app); err != nil {
		return "", fmt.Errorf("scope access token: %w", err)
	}
	return secret, nil
}

func newRefreshToken() (string, error) {
	tok, err := NewToken()
	if err != nil {
		return "", err
	}
	return RefreshTokenPrefix + tok, nil
}

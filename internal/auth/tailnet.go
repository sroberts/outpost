package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrAmbiguousTailnetLogin is a tailnet login that no user is linked to and
// more than one user has as their email, so it can't be said whose it is.
var ErrAmbiguousTailnetLogin = errors.New("tailnet login matches more than one user's email")

// NormalizeTailnetLogin is a Tailscale login name in the form it is stored
// and compared in. Tailscale's own comparison ignores case.
func NormalizeTailnetLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// validTailnetLogin accepts what Tailscale reports as a login name, which is
// always user@provider: an email address, or a name like alice@github.
func validTailnetLogin(login string) error {
	if err := validText("tailnet login", login); err != nil {
		return err
	}
	if strings.ContainsFunc(login, func(r rune) bool { return r == ' ' || r == '\t' }) || strings.Count(login, "@") != 1 ||
		strings.HasPrefix(login, "@") || strings.HasSuffix(login, "@") {
		return fmt.Errorf("tailnet login %q isn't a Tailscale login name, which looks like alice@example.com or alice@github", login)
	}
	return nil
}

// SetTailnetLogin links a Tailscale login to a user, so a connection from a
// device signed in to that login counts as them; "" removes the link. A login
// can be linked to one user at a time.
func (s *Store) SetTailnetLogin(username, login string) error {
	login = NormalizeTailnetLogin(login)
	if login != "" {
		if err := validTailnetLogin(login); err != nil {
			return err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return err
	}
	if login != "" {
		var other string
		err := tx.QueryRow(`SELECT username FROM users WHERE tailnet_login = ? AND id != ?`, login, id).Scan(&other)
		switch {
		case err == nil:
			return fmt.Errorf("tailnet login %s is already linked to %s; unlink it there first with `outpost user tailnet %s off`", login, other, other)
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("check tailnet login: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE users SET tailnet_login = ? WHERE id = ?`, login, id); err != nil {
		return fmt.Errorf("update %s: %w", username, err)
	}
	return tx.Commit()
}

// UserForTailnetLogin finds the user a Tailscale login belongs to. A user
// explicitly linked to it wins. Failing that, it is the one user with no link
// of their own whose email is that login: someone linked elsewhere has said
// which login is theirs, and two users sharing an email can't both be it.
//
// It returns the user whatever their state; the caller decides what a
// disabled one may do.
func (s *Store) UserForTailnetLogin(login string) (User, error) {
	login = NormalizeTailnetLogin(login)
	if login == "" {
		return User{}, fmt.Errorf("%w: no tailnet login", ErrNoSuchUser)
	}
	var username string
	err := s.db.QueryRow(`SELECT username FROM users WHERE tailnet_login = ?`, login).Scan(&username)
	if err == nil {
		return s.GetUser(username)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return User{}, fmt.Errorf("look up tailnet login: %w", err)
	}

	rows, err := s.db.Query(`SELECT username FROM users WHERE tailnet_login = '' AND lower(trim(email)) = ? LIMIT 2`, login)
	if err != nil {
		return User{}, fmt.Errorf("look up tailnet login by email: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return User{}, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return User{}, err
	}
	// The store has one connection; GetUser below needs it.
	rows.Close()
	switch len(names) {
	case 0:
		return User{}, fmt.Errorf("%w: no user is linked to tailnet login %s", ErrNoSuchUser, login)
	case 1:
		return s.GetUser(names[0])
	default:
		return User{}, fmt.Errorf("%w: %s; link it to one with `outpost user tailnet <name> %s`", ErrAmbiguousTailnetLogin, login, login)
	}
}

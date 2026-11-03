package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// User is one person who can log in, as callers see them. It carries no hash.
type User struct {
	Username    string
	DisplayName string
	Email       string
	Admin       bool
	Disabled    bool
	Groups      []string // sorted
	HasPassword bool
	CreatedAt   time.Time
	LastLoginAt time.Time
	Sessions    int // live sessions, for `user list`

	// TailnetLogin is the Tailscale login linked to this user, lower case,
	// "" if none; see UserForTailnetLogin.
	TailnetLogin string
}

var (
	ErrUserExists     = errors.New("user already exists")
	ErrNoSuchUser     = errors.New("no such user")
	ErrBadCredentials = errors.New("wrong username or password")
)

// lastAdminError explains a refusal that would leave nobody able to manage
// users. Recovering from that would mean editing auth.db by hand.
func lastAdminError(username, action string) error {
	return fmt.Errorf("refusing to %s %s: they are the last enabled admin; "+
		"make another user an admin first with `outpost user admin <name> on`", action, username)
}

// AddUser creates a user. An empty password creates one who can't log in
// until a password is set, so an account can be made before it's handed over.
func (s *Store) AddUser(u User, password string) error {
	if err := ValidUsername(u.Username); err != nil {
		return err
	}
	if err := validText("display name", u.DisplayName); err != nil {
		return err
	}
	if err := validText("email", u.Email); err != nil {
		return err
	}
	for _, g := range u.Groups {
		if err := ValidGroup(g); err != nil {
			return err
		}
	}
	hash := ""
	if password != "" {
		if err := ValidPassword(password); err != nil {
			return err
		}
		h, err := HashPassword(password, s.Params)
		if err != nil {
			return err
		}
		hash = h
	}

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	if _, ok, err := userID(tx, u.Username); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: %s", ErrUserExists, u.Username)
	}
	res, err := tx.Exec(`
INSERT INTO users (username, display_name, email, password_hash, is_admin, created_at)
VALUES (?, ?, ?, ?, ?, ?)`,
		u.Username, u.DisplayName, u.Email, hash, u.Admin, s.now().Unix())
	if err != nil {
		return fmt.Errorf("add user %s: %w", u.Username, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, g := range u.Groups {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO user_groups (user_id, name) VALUES (?, ?)`, id, g); err != nil {
			return fmt.Errorf("add %s to group %s: %w", u.Username, g, err)
		}
	}
	return tx.Commit()
}

// SetPassword replaces a user's password and ends every session and access
// token they have, so a password changed because it leaked actually locks the
// holder out: anyone who knew it could have had a token made in the meantime.
func (s *Store) SetPassword(username, password string) error {
	if err := ValidPassword(password); err != nil {
		return err
	}
	hash, err := HashPassword(password, s.Params)
	if err != nil {
		return err
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
	if _, err := tx.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, hash, id); err != nil {
		return fmt.Errorf("set password for %s: %w", username, err)
	}
	if err := endUserAccess(tx, id, username); err != nil {
		return err
	}
	return tx.Commit()
}

// RemoveUser deletes a user; their groups, sessions and access tokens go with
// them.
func (s *Store) RemoveUser(username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return err
	}
	if err := guardLastAdmin(tx, id, username, "remove"); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM users WHERE id = ?`, id); err != nil {
		return fmt.Errorf("remove user %s: %w", username, err)
	}
	return tx.Commit()
}

// SetDisabled turns a user's access off or back on. Disabling ends their
// sessions and access tokens; enabling doesn't restore them.
func (s *Store) SetDisabled(username string, disabled bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return err
	}
	if disabled {
		if err := guardLastAdmin(tx, id, username, "disable"); err != nil {
			return err
		}
		if err := endUserAccess(tx, id, username); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET disabled = ? WHERE id = ?`, disabled, id); err != nil {
		return fmt.Errorf("update %s: %w", username, err)
	}
	return tx.Commit()
}

// SetAdmin grants or revokes admin.
func (s *Store) SetAdmin(username string, admin bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return err
	}
	if !admin {
		if err := guardLastAdmin(tx, id, username, "demote"); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE users SET is_admin = ? WHERE id = ?`, admin, id); err != nil {
		return fmt.Errorf("update %s: %w", username, err)
	}
	return tx.Commit()
}

// UpdateGroups adds and removes group memberships and returns the result.
// Adding a group the user is already in, or removing one they aren't, is
// not an error.
func (s *Store) UpdateGroups(username string, add, remove []string) ([]string, error) {
	for _, g := range append(append([]string{}, add...), remove...) {
		if err := ValidGroup(g); err != nil {
			return nil, err
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollback(tx)

	id, err := mustUserID(tx, username)
	if err != nil {
		return nil, err
	}
	for _, g := range add {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO user_groups (user_id, name) VALUES (?, ?)`, id, g); err != nil {
			return nil, fmt.Errorf("add %s to %s: %w", username, g, err)
		}
	}
	for _, g := range remove {
		if _, err := tx.Exec(`DELETE FROM user_groups WHERE user_id = ? AND name = ?`, id, g); err != nil {
			return nil, fmt.Errorf("remove %s from %s: %w", username, g, err)
		}
	}
	groups, err := groupsOf(tx, id)
	if err != nil {
		return nil, err
	}
	return groups, tx.Commit()
}

// GetUser returns one user.
func (s *Store) GetUser(username string) (User, error) {
	r, err := s.getUser(s.db, username)
	if err != nil {
		return User{}, err
	}
	return r.User, nil
}

// ListUsers returns every user, sorted by username.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT ` + userCols + ` FROM users ORDER BY username`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	var recs []userRow
	for rows.Next() {
		r, err := scanUser(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		recs = append(recs, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// The store has one connection, so the cursor must be closed before the
	// per-user queries below can run.
	rows.Close()

	users := make([]User, 0, len(recs))
	for _, r := range recs {
		if err := s.fillUser(s.db, &r); err != nil {
			return nil, err
		}
		users = append(users, r.User)
	}
	return users, nil
}

// CountAdmins reports how many enabled admins there are.
func (s *Store) CountAdmins() (int, error) {
	return countAdmins(s.db)
}

// Authenticate checks a username and password. Every failure, whether the
// user is unknown, disabled, has no password or gave the wrong one, takes the
// same time and returns ErrBadCredentials, so the answer reveals nothing
// about which accounts exist.
func (s *Store) Authenticate(username, password string) (User, error) {
	r, err := s.getUser(s.db, username)
	if err != nil && !errors.Is(err, ErrNoSuchUser) {
		return User{}, err
	}
	if err != nil || r.hash == "" || r.Disabled {
		// Burn the same argon2 work a real check would, then fail.
		_, _ = VerifyPassword(password, s.dummyHash())
		return User{}, ErrBadCredentials
	}
	ok, err := VerifyPassword(password, r.hash)
	if err != nil {
		return User{}, fmt.Errorf("password hash for %s: %w", username, err)
	}
	if !ok {
		return User{}, ErrBadCredentials
	}

	now := s.now()
	if NeedsRehash(r.hash, s.Params) {
		s.rehash(r.id, r.hash, password)
	}
	if _, err := s.db.Exec(`UPDATE users SET last_login_at = ? WHERE id = ?`, now.Unix(), r.id); err != nil {
		return User{}, fmt.Errorf("record login for %s: %w", username, err)
	}
	r.LastLoginAt = fromUnix(now.Unix())
	return r.User, nil
}

// rehash upgrades a hash to the current parameters after a successful login.
// It only replaces the hash that was verified: argon2 takes long enough that
// `outpost user passwd` can land in between, and an unconditional write would
// put the old password back. A failed upgrade isn't a failed login; the old
// hash still works and the next login tries again.
func (s *Store) rehash(id int64, verified, password string) {
	h, err := HashPassword(password, s.Params)
	if err != nil {
		return
	}
	_, _ = s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?`, h, id, verified)
}

// dummyHash is verified against when there is no real hash to check, so an
// unknown user costs the same as a known one. It's made once, with the
// store's parameters, so the cost matches.
func (s *Store) dummyHash() string {
	s.dummyOnce.Do(func() {
		h, err := HashPassword("outpost-dummy-password", s.Params)
		if err != nil {
			h, _ = HashPassword("outpost-dummy-password", DefaultParams)
		}
		s.dummy = h
	})
	return s.dummy
}

// querier is what both *sql.DB and *sql.Tx provide.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// userRow is a User plus the columns callers never see.
type userRow struct {
	User
	id   int64
	hash string
}

const userCols = `id, username, display_name, email, password_hash, is_admin, disabled, created_at, last_login_at, tailnet_login`

type scanner interface{ Scan(dest ...any) error }

func scanUser(sc scanner) (userRow, error) {
	var (
		r                userRow
		created, lastLog int64
	)
	if err := sc.Scan(&r.id, &r.Username, &r.DisplayName, &r.Email, &r.hash,
		&r.Admin, &r.Disabled, &created, &lastLog, &r.TailnetLogin); err != nil {
		return userRow{}, err
	}
	r.HasPassword = r.hash != ""
	r.CreatedAt = fromUnix(created)
	r.LastLoginAt = fromUnix(lastLog)
	return r, nil
}

func (s *Store) getUser(q querier, username string) (userRow, error) {
	r, err := scanUser(q.QueryRow(`SELECT `+userCols+` FROM users WHERE username = ?`, username))
	if errors.Is(err, sql.ErrNoRows) {
		return userRow{}, fmt.Errorf("%w: %s", ErrNoSuchUser, username)
	}
	if err != nil {
		return userRow{}, fmt.Errorf("get user %s: %w", username, err)
	}
	if err := s.fillUser(q, &r); err != nil {
		return userRow{}, err
	}
	return r, nil
}

// fillUser adds the groups and the live-session count.
func (s *Store) fillUser(q querier, r *userRow) error {
	groups, err := groupsOf(q, r.id)
	if err != nil {
		return err
	}
	r.Groups = groups
	if err := q.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ?`,
		r.id, s.now().Unix()).Scan(&r.Sessions); err != nil {
		return fmt.Errorf("count sessions for %s: %w", r.Username, err)
	}
	return nil
}

func groupsOf(q querier, id int64) ([]string, error) {
	rows, err := q.Query(`SELECT name FROM user_groups WHERE user_id = ? ORDER BY name`, id)
	if err != nil {
		return nil, fmt.Errorf("load groups: %w", err)
	}
	defer rows.Close()
	groups := []string{}
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func userID(q querier, username string) (int64, bool, error) {
	var id int64
	err := q.QueryRow(`SELECT id FROM users WHERE username = ?`, username).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("look up %s: %w", username, err)
	}
	return id, true, nil
}

func mustUserID(q querier, username string) (int64, error) {
	id, ok, err := userID(q, username)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrNoSuchUser, username)
	}
	return id, nil
}

func countAdmins(q querier) (int, error) {
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM users WHERE is_admin = 1 AND disabled = 0`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return n, nil
}

// guardLastAdmin refuses an action that would leave no enabled admin.
func guardLastAdmin(q querier, id int64, username, action string) error {
	var admin, disabled bool
	if err := q.QueryRow(`SELECT is_admin, disabled FROM users WHERE id = ?`, id).Scan(&admin, &disabled); err != nil {
		return fmt.Errorf("look up %s: %w", username, err)
	}
	if !admin || disabled {
		return nil
	}
	n, err := countAdmins(q)
	if err != nil {
		return err
	}
	if n <= 1 {
		return lastAdminError(username, action)
	}
	return nil
}

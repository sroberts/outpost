package auth

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

const pw = "correct horse battery"

func mustAdd(t *testing.T, s *Store, u User, password string) {
	t.Helper()
	if err := s.AddUser(u, password); err != nil {
		t.Fatalf("AddUser(%s): %v", u.Username, err)
	}
}

func TestUsers_AddGetList(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "bob", DisplayName: "Bob", Email: "bob@example.com",
		Groups: []string{"kids", "family"}}, pw)
	mustAdd(t, s, User{Username: "alice", Admin: true}, "")

	bob, err := s.GetUser("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.DisplayName != "Bob" || bob.Email != "bob@example.com" || bob.Admin || bob.Disabled {
		t.Errorf("GetUser(bob) = %+v", bob)
	}
	if !reflect.DeepEqual(bob.Groups, []string{"family", "kids"}) {
		t.Errorf("groups = %v; want them sorted", bob.Groups)
	}
	if !bob.HasPassword {
		t.Error("bob.HasPassword = false")
	}
	if !bob.CreatedAt.Equal(c.t) {
		t.Errorf("CreatedAt = %v; want %v", bob.CreatedAt, c.t)
	}
	if !bob.LastLoginAt.IsZero() {
		t.Errorf("LastLoginAt = %v before any login", bob.LastLoginAt)
	}

	users, err := s.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("ListUsers = %+v; want alice then bob", users)
	}
	if users[0].HasPassword {
		t.Error("alice was added without a password but HasPassword = true")
	}
	if !users[0].Admin {
		t.Error("alice lost her admin flag")
	}
	if users[0].Groups == nil {
		t.Error("a user with no groups should have an empty slice, not nil")
	}
}

func TestUsers_AddRejectsBadInput(t *testing.T) {
	s, _ := newTestStore(t)
	for _, c := range []struct {
		u  User
		pw string
	}{
		{User{Username: "Alice"}, pw},
		{User{Username: "alice", Groups: []string{"Bad Group"}}, pw},
		{User{Username: "alice"}, "short"},
		{User{Username: "alice", DisplayName: "A\r\nB"}, pw},
	} {
		if err := s.AddUser(c.u, c.pw); err == nil {
			t.Errorf("AddUser(%+v) accepted bad input", c.u)
		}
	}
	if users, _ := s.ListUsers(); len(users) != 0 {
		t.Errorf("rejected adds left %d users behind", len(users))
	}
}

func TestUsers_DuplicateAdd(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	if err := s.AddUser(User{Username: "alice"}, pw); !errors.Is(err, ErrUserExists) {
		t.Errorf("second AddUser = %v; want ErrUserExists", err)
	}
}

func TestUsers_UnknownUser(t *testing.T) {
	s, _ := newTestStore(t)
	for name, err := range map[string]error{
		"GetUser":     func() error { _, err := s.GetUser("ghost"); return err }(),
		"SetPassword": s.SetPassword("ghost", pw),
		"RemoveUser":  s.RemoveUser("ghost"),
		"SetDisabled": s.SetDisabled("ghost", true),
		"SetAdmin":    s.SetAdmin("ghost", true),
		"UpdateGroups": func() error {
			_, err := s.UpdateGroups("ghost", []string{"x"}, nil)
			return err
		}(),
	} {
		if !errors.Is(err, ErrNoSuchUser) {
			t.Errorf("%s(ghost) = %v; want ErrNoSuchUser", name, err)
		}
	}
}

func liveSessions(t *testing.T, s *Store, username string) int {
	t.Helper()
	u, err := s.GetUser(username)
	if err != nil {
		t.Fatal(err)
	}
	return u.Sessions
}

func TestUsers_SetPasswordEndsSessions(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	tok := mustSession(t, s, "alice")

	if err := s.SetPassword("alice", "a whole new passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("session survived a password change: %v", err)
	}
	if _, err := s.Authenticate("alice", pw); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old password still works: %v", err)
	}
	if _, err := s.Authenticate("alice", "a whole new passphrase"); err != nil {
		t.Errorf("new password doesn't work: %v", err)
	}
	if err := s.SetPassword("alice", "short"); err == nil {
		t.Error("SetPassword accepted a short password")
	}
}

func TestUsers_RemoveCascades(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice", Groups: []string{"family"}}, pw)
	tok := mustSession(t, s, "alice")

	if err := s.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("session survived removal: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM sessions) + (SELECT COUNT(*) FROM user_groups)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d session or group rows left behind by RemoveUser", n)
	}
	// The name is free again.
	mustAdd(t, s, User{Username: "alice"}, pw)
}

func TestUsers_DisableEndsSessionsAndBlocksLogin(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	tok := mustSession(t, s, "alice")

	if err := s.SetDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("session survived disabling: %v", err)
	}
	if _, err := s.Authenticate("alice", pw); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("disabled user authenticated: %v", err)
	}
	if _, err := s.CreateSession("alice", "", "", ""); err == nil {
		t.Error("CreateSession succeeded for a disabled user")
	}

	if err := s.SetDisabled("alice", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate("alice", pw); err != nil {
		t.Errorf("re-enabled user can't log in: %v", err)
	}
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Error("re-enabling brought an old session back")
	}
}

func TestUsers_LastAdminGuards(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice", Admin: true}, pw)
	mustAdd(t, s, User{Username: "bob"}, pw)

	for name, err := range map[string]error{
		"remove":  s.RemoveUser("alice"),
		"disable": s.SetDisabled("alice", true),
		"demote":  s.SetAdmin("alice", false),
	} {
		if err == nil || !strings.Contains(err.Error(), "last enabled admin") {
			t.Errorf("%s the last admin: %v; want a refusal naming the rule", name, err)
		}
	}
	if u, _ := s.GetUser("alice"); !u.Admin || u.Disabled {
		t.Fatalf("a refused change still changed alice: %+v", u)
	}

	// A disabled admin doesn't count: with bob an admin but disabled, alice is
	// still the last one who can act.
	if err := s.SetAdmin("bob", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDisabled("bob", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdmin("alice", false); err == nil {
		t.Error("demoted alice while the only other admin is disabled")
	}

	// With a second enabled admin, each operation goes through.
	if err := s.SetDisabled("bob", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdmin("alice", false); err != nil {
		t.Errorf("demote with another admin: %v", err)
	}
	if n, _ := s.CountAdmins(); n != 1 {
		t.Errorf("CountAdmins = %d; want 1", n)
	}
	// Non-admins are never guarded.
	if err := s.RemoveUser("alice"); err != nil {
		t.Errorf("remove a non-admin: %v", err)
	}
}

func TestUsers_UpdateGroups(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice", Groups: []string{"kids"}}, pw)

	got, err := s.UpdateGroups("alice", []string{"family", "admins", "family"}, []string{"kids", "never-was"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"admins", "family"}) {
		t.Errorf("UpdateGroups = %v; want [admins family]", got)
	}
	if _, err := s.UpdateGroups("alice", []string{"Bad"}, nil); err == nil {
		t.Error("UpdateGroups accepted an invalid group name")
	}
}

func TestAuthenticate_SameErrorForUnknownAndWrong(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	mustAdd(t, s, User{Username: "nopass"}, "")

	_, unknown := s.Authenticate("ghost", pw)
	_, wrong := s.Authenticate("alice", "incorrect horse battery")
	_, noPass := s.Authenticate("nopass", "")
	for name, err := range map[string]error{"unknown user": unknown, "wrong password": wrong, "no password set": noPass} {
		if !errors.Is(err, ErrBadCredentials) {
			t.Errorf("%s: %v; want ErrBadCredentials", name, err)
		}
	}
	if unknown.Error() != wrong.Error() {
		t.Errorf("messages differ: %q vs %q", unknown, wrong)
	}
}

func TestAuthenticate_RecordsLogin(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice", Groups: []string{"family"}}, pw)
	c.advance(time.Hour)

	u, err := s.Authenticate("alice", pw)
	if err != nil {
		t.Fatal(err)
	}
	if !u.LastLoginAt.Equal(c.t) {
		t.Errorf("returned LastLoginAt = %v; want %v", u.LastLoginAt, c.t)
	}
	if !reflect.DeepEqual(u.Groups, []string{"family"}) {
		t.Errorf("returned groups = %v", u.Groups)
	}
	stored, _ := s.GetUser("alice")
	if !stored.LastLoginAt.Equal(c.t) {
		t.Errorf("stored LastLoginAt = %v; want %v", stored.LastLoginAt, c.t)
	}
}

func TestAuthenticate_RehashesWhenParamsChange(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	before := storedHash(t, s, "alice")

	s.Params.Time = 2
	if _, err := s.Authenticate("alice", pw); err != nil {
		t.Fatal(err)
	}
	after := storedHash(t, s, "alice")
	if after == before {
		t.Fatal("hash unchanged after a login with new parameters")
	}
	if NeedsRehash(after, s.Params) {
		t.Errorf("new hash %q doesn't use the new parameters", after)
	}
	if _, err := s.Authenticate("alice", pw); err != nil {
		t.Errorf("can't log in with the rehashed password: %v", err)
	}
	// A failed login must never rewrite the hash.
	if _, err := s.Authenticate("alice", "incorrect horse battery"); err == nil {
		t.Fatal("wrong password accepted")
	}
	if storedHash(t, s, "alice") != after {
		t.Error("a failed login changed the stored hash")
	}
}

func storedHash(t *testing.T, s *Store, username string) string {
	t.Helper()
	var h string
	if err := s.db.QueryRow(`SELECT password_hash FROM users WHERE username = ?`, username).Scan(&h); err != nil {
		t.Fatal(err)
	}
	return h
}

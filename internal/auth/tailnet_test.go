package auth

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func tailnetStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTailnetLogin_LinkWinsOverEmail(t *testing.T) {
	s := tailnetStore(t)
	for _, u := range []User{
		{Username: "alice", Email: "shared@example.com"},
		{Username: "bob"},
	} {
		if err := s.AddUser(u, ""); err != nil {
			t.Fatal(err)
		}
	}
	if u, err := s.UserForTailnetLogin("Shared@Example.com"); err != nil || u.Username != "alice" {
		t.Fatalf("by email: %q, %v; want alice", u.Username, err)
	}
	// Bob says the login is his; that beats alice's email.
	if err := s.SetTailnetLogin("bob", " SHARED@example.com "); err != nil {
		t.Fatal(err)
	}
	u, err := s.UserForTailnetLogin("shared@example.com")
	if err != nil || u.Username != "bob" {
		t.Fatalf("linked: %q, %v; want bob", u.Username, err)
	}
	if u.TailnetLogin != "shared@example.com" {
		t.Errorf("stored as %q; want it lower case and trimmed", u.TailnetLogin)
	}
}

func TestTailnetLogin_EmailOnlyForUsersWithoutALink(t *testing.T) {
	s := tailnetStore(t)
	if err := s.AddUser(User{Username: "carol", Email: "carol@example.com"}, ""); err != nil {
		t.Fatal(err)
	}
	// Carol has said which login is hers, so her email no longer counts.
	if err := s.SetTailnetLogin("carol", "carol@github"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForTailnetLogin("carol@example.com"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("email of a linked user: err = %v; want ErrNoSuchUser", err)
	}
	if u, err := s.UserForTailnetLogin("carol@github"); err != nil || u.Username != "carol" {
		t.Errorf("her link: %q, %v", u.Username, err)
	}
}

func TestTailnetLogin_SharedEmailIsAmbiguous(t *testing.T) {
	s := tailnetStore(t)
	for _, name := range []string{"dan", "dana"} {
		if err := s.AddUser(User{Username: name, Email: "team@example.com"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UserForTailnetLogin("team@example.com"); !errors.Is(err, ErrAmbiguousTailnetLogin) {
		t.Fatalf("err = %v; want ErrAmbiguousTailnetLogin", err)
	}
	// A link settles it.
	if err := s.SetTailnetLogin("dana", "team@example.com"); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UserForTailnetLogin("team@example.com"); err != nil || u.Username != "dana" {
		t.Errorf("after linking: %q, %v; want dana", u.Username, err)
	}
}

func TestTailnetLogin_OneUserPerLogin(t *testing.T) {
	s := tailnetStore(t)
	for _, name := range []string{"erin", "frank"} {
		if err := s.AddUser(User{Username: name}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTailnetLogin("erin", "erin@github"); err != nil {
		t.Fatal(err)
	}
	err := s.SetTailnetLogin("frank", "ERIN@github")
	if err == nil || !strings.Contains(err.Error(), "already linked to erin") {
		t.Fatalf("second link: err = %v; want it refused, naming erin", err)
	}
	// Relinking her own login, and unlinking, are fine; then frank may have it.
	if err := s.SetTailnetLogin("erin", "erin@github"); err != nil {
		t.Errorf("relink: %v", err)
	}
	if err := s.SetTailnetLogin("erin", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTailnetLogin("frank", "erin@github"); err != nil {
		t.Errorf("after unlink: %v", err)
	}
}

func TestTailnetLogin_Validation(t *testing.T) {
	s := tailnetStore(t)
	if err := s.AddUser(User{Username: "gail"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"gail", "@github", "gail@", "a@b@c", "gail @github", "gail@git\nhub"} {
		if err := s.SetTailnetLogin("gail", bad); err == nil {
			t.Errorf("SetTailnetLogin(%q) accepted", bad)
		}
	}
	if err := s.SetTailnetLogin("nobody", "x@github"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("unknown user: err = %v; want ErrNoSuchUser", err)
	}
}

// A database made before tailnet logins gains the column, and the uniqueness
// that goes with it, on open.
func TestTailnetLogin_MigratesAnOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    email         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    is_admin      INTEGER NOT NULL DEFAULT 0,
    disabled      INTEGER NOT NULL DEFAULT 0,
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO users (username, email, created_at) VALUES ('old', 'old@example.com', 1);`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if u, err := s.UserForTailnetLogin("old@example.com"); err != nil || u.Username != "old" {
		t.Fatalf("after migrating: %q, %v", u.Username, err)
	}
	if err := s.SetTailnetLogin("old", "old@github"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO users (username, created_at, tailnet_login) VALUES ('dup', 1, 'old@github')`); err == nil {
		t.Error("the migrated table let two users share a tailnet login")
	}
}

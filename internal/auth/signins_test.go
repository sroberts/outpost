package auth

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func signInStore(t *testing.T, users ...string) (*Store, *clock) {
	t.Helper()
	s, c := newTestStore(t)
	for _, u := range users {
		if err := s.AddUser(User{Username: u}, ""); err != nil {
			t.Fatal(err)
		}
	}
	return s, c
}

func TestSignInsGroupAppSessionsUnderTheirLogin(t *testing.T) {
	s, _ := signInStore(t, "alice", "bob")
	phone, err := s.CreateSession("alice", "", "", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("alice", "", "", "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("bob", "", "", "bobs"); err != nil {
		t.Fatal(err)
	}
	for _, app := range []string{"yarr", "wiki", "yarr"} {
		if _, err := s.CreateSession("alice", app, phone, "phone"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.SignIns("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sign-ins, want 2: %+v", len(got), got)
	}
	var in SignIn
	for _, g := range got {
		if g.UserAgent == "bobs" {
			t.Error("listed another user's sign-in")
		}
		if g.ID == HashToken(phone) {
			in = g
		}
	}
	if in.UserAgent != "phone" || len(in.Apps) != 2 || in.Apps[0] != "wiki" || in.Apps[1] != "yarr" {
		t.Errorf("phone's sign-in = %+v; want its two apps, each once and sorted", in)
	}
}

func TestSignInsLeaveOutExpiredAndSortByUse(t *testing.T) {
	s, c := signInStore(t, "alice")
	if _, err := s.CreateSession("alice", "", "", "older"); err != nil {
		t.Fatal(err)
	}
	c.advance(2 * 24 * time.Hour)
	if _, err := s.CreateSession("alice", "", "", "newer"); err != nil {
		t.Fatal(err)
	}

	got, err := s.SignIns("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserAgent != "newer" {
		t.Fatalf("got %+v; want the most recently used first", got)
	}

	c.advance(PortalSessionTTL - 2*24*time.Hour + time.Hour)
	if got, err = s.SignIns("alice"); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].UserAgent != "newer" {
		t.Errorf("got %+v; want the expired sign-in left out", got)
	}
}

func TestEndSignInEndsOneLoginOnly(t *testing.T) {
	s, _ := signInStore(t, "alice")
	lost, _ := s.CreateSession("alice", "", "", "lost")
	lostApp, _ := s.CreateSession("alice", "yarr", lost, "lost")
	kept, _ := s.CreateSession("alice", "", "", "kept")
	keptApp, _ := s.CreateSession("alice", "yarr", kept, "kept")

	if err := s.EndSignIn("alice", HashToken(lost)); err != nil {
		t.Fatalf("EndSignIn: %v", err)
	}
	for _, tok := range []string{lost, lostApp} {
		if _, _, err := s.LookupSession(tok); err == nil {
			t.Error("a session of the ended login still works")
		}
	}
	for _, tok := range []string{kept, keptApp} {
		if _, _, err := s.LookupSession(tok); err != nil {
			t.Errorf("a session of the other login ended: %v", err)
		}
	}
	if err := s.EndSignIn("alice", HashToken(lost)); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("ending it again = %v; want ErrNoSuchSession, nothing to end", err)
	}
}

// Each of these names something the caller has no business ending.
func TestEndSignInRefuses(t *testing.T) {
	s, _ := signInStore(t, "alice", "bob")
	mine, _ := s.CreateSession("alice", "", "", "alices")
	app, _ := s.CreateSession("alice", "yarr", mine, "alices")

	for name, tc := range map[string]struct{ user, id string }{
		"another user's login": {"bob", HashToken(mine)},
		"an empty ID":          {"alice", ""},
		"an app session":       {"alice", HashToken(app)},
		"a token, not its ID":  {"alice", mine},
		"a user who isn't":     {"nobody", HashToken(mine)},
	} {
		if err := s.EndSignIn(tc.user, tc.id); !errors.Is(err, ErrNoSuchSession) {
			t.Errorf("%s: err = %v; want ErrNoSuchSession", name, err)
		}
	}
	for _, tok := range []string{mine, app} {
		if _, _, err := s.LookupSession(tok); err != nil {
			t.Errorf("a session ended anyway: %v", err)
		}
	}
}

// The address a sign-in came from is kept and listed with it.
func TestSignInsKeepTheAddress(t *testing.T) {
	s, _ := signInStore(t, "alice")
	if _, err := s.CreateSessionFrom("alice", "", "", "x", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SignIns("alice")
	if err != nil || len(got) != 1 || got[0].IP != "192.0.2.1" {
		t.Fatalf("SignIns = %+v, %v", got, err)
	}
	if _, err := s.SignIns("nobody"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("an unknown user = %v; want ErrNoSuchUser", err)
	}
}

// Ending the others keeps the one in use, and its apps.
func TestEndOtherSignIns(t *testing.T) {
	s, _ := signInStore(t, "alice", "bob")
	here, _ := s.CreateSession("alice", "", "", "here")
	hereApp, _ := s.CreateSession("alice", "yarr", here, "here")
	lost, _ := s.CreateSession("alice", "", "", "lost")
	lostApp, _ := s.CreateSession("alice", "yarr", lost, "lost")
	other, _ := s.CreateSession("alice", "", "", "other")
	bobs, _ := s.CreateSession("bob", "", "", "bobs")

	n, err := s.EndOtherSignIns("alice", HashToken(here))
	if err != nil || n != 2 {
		t.Fatalf("EndOtherSignIns = %d, %v; want 2", n, err)
	}
	for _, tok := range []string{lost, lostApp, other} {
		if _, _, err := s.LookupSession(tok); err == nil {
			t.Error("another sign-in survived")
		}
	}
	for _, tok := range []string{here, hereApp, bobs} {
		if _, _, err := s.LookupSession(tok); err != nil {
			t.Errorf("a kept session ended: %v", err)
		}
	}
}

// An auth.db from before addresses were kept gains the column on open, and
// its sessions still list.
func TestOpen_AddsSessionIPToAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old := strings.Replace(schema, ",\n    ip           TEXT NOT NULL DEFAULT ''", "", 1)
	if old == schema {
		t.Fatal("the test no longer knows how to build the old schema")
	}
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Params = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
	if err := s.AddUser(User{Username: "alice"}, pw); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSessionFrom("alice", "", "", "x", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	list, err := s.SignIns("alice")
	if err != nil || len(list) != 1 || list[0].IP != "192.0.2.1" {
		t.Fatalf("SignIns = %+v, %v", list, err)
	}
	// Opening again finds the column there and leaves it.
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

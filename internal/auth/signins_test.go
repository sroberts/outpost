package auth

import (
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

	ended, err := s.EndSignIn("alice", HashToken(lost))
	if err != nil || !ended {
		t.Fatalf("EndSignIn = %v, %v", ended, err)
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
	if ended, err := s.EndSignIn("alice", HashToken(lost)); err != nil || ended {
		t.Errorf("ending it again = %v, %v; want nothing to end and no error", ended, err)
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
		if ended, err := s.EndSignIn(tc.user, tc.id); err != nil || ended {
			t.Errorf("%s: ended = %v, err = %v", name, ended, err)
		}
	}
	for _, tok := range []string{mine, app} {
		if _, _, err := s.LookupSession(tok); err != nil {
			t.Errorf("a session ended anyway: %v", err)
		}
	}
}

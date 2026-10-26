package auth

import (
	"errors"
	"testing"
)

// Using an app keeps the login alive: the portal session slides with its
// app session, so someone who never revisits the portal stays logged in.
func TestTouchFamily_SlidesTheParent(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	portal := mustSession(t, s, "alice")
	yarr, err := s.CreateSession("alice", "yarr", portal, "")
	if err != nil {
		t.Fatal(err)
	}

	c.advance(PortalSessionTTL / 2)
	if err := s.TouchFamily(yarr); err != nil {
		t.Fatal(err)
	}
	c.advance(PortalSessionTTL / 2)
	if _, _, err := s.LookupSession(yarr); err != nil {
		t.Errorf("app session died although it was in use: %v", err)
	}
	if _, _, err := s.LookupSession(portal); err != nil {
		t.Errorf("portal session wasn't slid by its app session: %v", err)
	}
}

func TestTouchFamily_PortalSessionAndExpiry(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	portal := mustSession(t, s, "alice")

	c.advance(PortalSessionTTL / 2)
	if err := s.TouchFamily(portal); err != nil {
		t.Fatal(err)
	}
	c.advance(PortalSessionTTL / 2)
	if _, _, err := s.LookupSession(portal); err != nil {
		t.Errorf("a touched portal session expired: %v", err)
	}

	c.advance(PortalSessionTTL)
	if err := s.TouchFamily(portal); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("TouchFamily on an expired session = %v; want ErrNoSuchSession", err)
	}
}

// Logging out from one app host ends the whole login, but not the user's
// other logins elsewhere.
func TestRevokeFamily(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	portal := mustSession(t, s, "alice")
	yarr, err := s.CreateSession("alice", "yarr", portal, "")
	if err != nil {
		t.Fatal(err)
	}
	wiki, err := s.CreateSession("alice", "wiki", portal, "")
	if err != nil {
		t.Fatal(err)
	}
	phone := mustSession(t, s, "alice")

	if err := s.RevokeFamily(yarr); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"portal": portal, "yarr": yarr, "wiki": wiki} {
		if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
			t.Errorf("%s session survived RevokeFamily: %v", name, err)
		}
	}
	if _, _, err := s.LookupSession(phone); err != nil {
		t.Errorf("an unrelated login was ended: %v", err)
	}

	// Twice, or with a token that was never issued, is fine.
	if err := s.RevokeFamily(yarr); err != nil {
		t.Errorf("second RevokeFamily: %v", err)
	}
	if err := s.RevokeFamily("never-issued"); err != nil {
		t.Errorf("RevokeFamily(unknown): %v", err)
	}
}

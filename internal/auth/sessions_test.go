package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustSession(t *testing.T, s *Store, username string) string {
	t.Helper()
	tok, err := s.CreateSession(username, "", "", "test-agent")
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", username, err)
	}
	return tok
}

func TestSessions_CreateAndLookup(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice", Groups: []string{"family"}}, pw)
	tok := mustSession(t, s, "alice")

	sess, u, err := s.LookupSession(tok)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Username != "alice" || sess.App != "" || u.Username != "alice" || len(u.Groups) != 1 {
		t.Errorf("LookupSession = %+v, %+v", sess, u)
	}
	if want := c.t.Add(PortalSessionTTL); !sess.ExpiresAt.Equal(want) {
		t.Errorf("ExpiresAt = %v; want %v", sess.ExpiresAt, want)
	}
	if u.Sessions != 1 {
		t.Errorf("user.Sessions = %d; want 1", u.Sessions)
	}

	// Only the hash is stored.
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash = ?`, tok).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("the raw token is stored in the database")
	}

	if _, _, err := s.LookupSession("not-a-token"); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("LookupSession(unknown) = %v; want ErrNoSuchSession", err)
	}
	if _, err := s.CreateSession("ghost", "", "", ""); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("CreateSession(ghost) = %v; want ErrNoSuchUser", err)
	}
}

func TestSessions_Expiry(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	tok := mustSession(t, s, "alice")

	c.advance(PortalSessionTTL - time.Second)
	if _, _, err := s.LookupSession(tok); err != nil {
		t.Fatalf("session expired early: %v", err)
	}
	c.advance(time.Second)
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("session alive at its expiry: %v", err)
	}
	if err := s.TouchSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("TouchSession revived an expired session: %v", err)
	}
	if u, _ := s.GetUser("alice"); u.Sessions != 0 {
		t.Errorf("user.Sessions = %d after expiry; want 0", u.Sessions)
	}
}

func TestSessions_TouchSlidesOnlyAfterRefreshAfter(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	tok := mustSession(t, s, "alice")
	created := c.t

	c.advance(RefreshAfter - time.Second)
	if err := s.TouchSession(tok); err != nil {
		t.Fatal(err)
	}
	sess, _, _ := s.LookupSession(tok)
	if want := created.Add(PortalSessionTTL); !sess.ExpiresAt.Equal(want) {
		t.Errorf("touch inside RefreshAfter moved expiry to %v; want it left at %v", sess.ExpiresAt, want)
	}

	c.advance(time.Second)
	if err := s.TouchSession(tok); err != nil {
		t.Fatal(err)
	}
	sess, _, _ = s.LookupSession(tok)
	if want := c.t.Add(PortalSessionTTL); !sess.ExpiresAt.Equal(want) {
		t.Errorf("touch after RefreshAfter left expiry at %v; want %v", sess.ExpiresAt, want)
	}

	// A session used daily never expires.
	for range 60 {
		c.advance(RefreshAfter)
		if err := s.TouchSession(tok); err != nil {
			t.Fatalf("daily-used session expired: %v", err)
		}
	}
}

func TestSessions_RevokeParentRevokesChildren(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	portal := mustSession(t, s, "alice")
	yarr, err := s.CreateSession("alice", "yarr", portal, "")
	if err != nil {
		t.Fatal(err)
	}
	other := mustSession(t, s, "alice")

	sess, _, err := s.LookupSession(yarr)
	if err != nil || sess.App != "yarr" {
		t.Fatalf("app session lookup = %+v, %v", sess, err)
	}

	if err := s.RevokeSession(portal); err != nil {
		t.Fatal(err)
	}
	for name, tok := range map[string]string{"portal": portal, "app": yarr} {
		if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
			t.Errorf("%s session survived revoking the portal session: %v", name, err)
		}
	}
	if _, _, err := s.LookupSession(other); err != nil {
		t.Errorf("an unrelated session was revoked too: %v", err)
	}
	if err := s.RevokeSession(portal); err != nil {
		t.Errorf("revoking twice: %v", err)
	}
}

func TestSessions_AppSessionRules(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	mustAdd(t, s, User{Username: "bob"}, pw)
	alice := mustSession(t, s, "alice")

	if _, err := s.CreateSession("alice", "yarr", "", ""); err == nil {
		t.Error("app session without a parent was created")
	}
	if _, err := s.CreateSession("alice", "", alice, ""); err == nil {
		t.Error("portal session with a parent was created")
	}
	if _, err := s.CreateSession("bob", "yarr", alice, ""); err == nil {
		t.Error("bob got an app session from alice's portal session")
	}
	yarr, err := s.CreateSession("alice", "yarr", alice, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("alice", "other", yarr, ""); err == nil {
		t.Error("an app session was used as a parent")
	}

	// The parent slides with use; a child is only as alive as its parent.
	c.advance(PortalSessionTTL / 2)
	if err := s.TouchSession(yarr); err != nil {
		t.Fatal(err)
	}
	c.advance(PortalSessionTTL / 2)
	if _, _, err := s.LookupSession(yarr); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("app session outlived its expired parent: %v", err)
	}
}

func TestSessions_DisabledUserDoesNotLookUp(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	tok := mustSession(t, s, "alice")
	// Flip the flag directly, past SetDisabled's own cleanup, to prove the
	// lookup checks it rather than relying on the rows being gone.
	if _, err := s.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
		t.Errorf("disabled user's session looked up: %v", err)
	}
}

func TestSessions_RevokeUserSessions(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	mustAdd(t, s, User{Username: "bob"}, pw)
	stale := mustSession(t, s, "alice")
	c.advance(PortalSessionTTL)
	a1 := mustSession(t, s, "alice")
	if _, err := s.CreateSession("alice", "yarr", a1, ""); err != nil {
		t.Fatal(err)
	}
	b := mustSession(t, s, "bob")

	n, err := s.RevokeUserSessions("alice")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("RevokeUserSessions = %d; want 2 (the expired one doesn't count)", n)
	}
	for _, tok := range []string{stale, a1} {
		if _, _, err := s.LookupSession(tok); !errors.Is(err, ErrNoSuchSession) {
			t.Error("alice still has a session")
		}
	}
	if _, _, err := s.LookupSession(b); err != nil {
		t.Errorf("bob's session went too: %v", err)
	}
	if _, err := s.RevokeUserSessions("ghost"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("RevokeUserSessions(ghost) = %v; want ErrNoSuchUser", err)
	}
}

func TestSessions_PruneExpired(t *testing.T) {
	s, c := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	old := mustSession(t, s, "alice")
	child, err := s.CreateSession("alice", "yarr", old, "")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(PortalSessionTTL / 2)
	if err := s.TouchSession(child); err != nil {
		t.Fatal(err)
	}
	keep := mustSession(t, s, "alice")
	c.advance(PortalSessionTTL / 2)

	// old has expired. Its child was touched along the way so its own expiry
	// is still ahead, but with the parent gone it has to go too.
	n, err := s.PruneExpired()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("PruneExpired removed %d rows; want 2", n)
	}
	if _, _, err := s.LookupSession(keep); err != nil {
		t.Errorf("a live session was pruned: %v", err)
	}
	if n, _ := s.PruneExpired(); n != 0 {
		t.Errorf("second prune removed %d rows; want 0", n)
	}
}

func TestSessions_TruncatesUserAgent(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	if _, err := s.CreateSession("alice", "", "", strings.Repeat("x", 10_000)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT MAX(LENGTH(user_agent)) FROM sessions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != maxUserAgent {
		t.Errorf("stored user agent is %d bytes; want %d", n, maxUserAgent)
	}
}

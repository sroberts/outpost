package auth

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A password change can land while a login is still verifying the old hash.
// The login's upgrade must not then write the old password back.
func TestAuthenticate_RehashLeavesANewerPasswordAlone(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	verified := storedHash(t, s, "alice")

	// The interleaving: passwd commits, then the in-flight login upgrades the
	// hash it verified.
	if err := s.SetPassword("alice", "a brand new passphrase"); err != nil {
		t.Fatal(err)
	}
	s.Params.Time = 2
	u, err := s.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := s.db.QueryRow(`SELECT id FROM users WHERE username = ?`, u.Username).Scan(&id); err != nil {
		t.Fatal(err)
	}
	s.rehash(id, verified, pw)

	if _, err := s.Authenticate("alice", "a brand new passphrase"); err != nil {
		t.Errorf("the new password stopped working: %v", err)
	}
	if _, err := s.Authenticate("alice", pw); err == nil {
		t.Error("the old password works again")
	}
}

// The rehash still happens when nothing changed underneath it.
func TestAuthenticate_RehashStillUpgrades(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	s.Params.Time = 2
	if _, err := s.Authenticate("alice", pw); err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(storedHash(t, s, "alice"), s.Params) {
		t.Error("the hash was not upgraded to the new parameters")
	}
}

// A user agent cut to length must not end in half a UTF-8 sequence.
func TestSessions_UserAgentCutAtACharacterBoundary(t *testing.T) {
	s, _ := newTestStore(t)
	mustAdd(t, s, User{Username: "alice"}, pw)
	// One ASCII byte, then two-byte runes: byte maxUserAgent falls mid-rune.
	ua := "x" + strings.Repeat("é", maxUserAgent)
	if _, err := s.CreateSession("alice", "", "", ua); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT user_agent FROM sessions`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(stored) {
		t.Errorf("stored user agent is not valid UTF-8: %q", stored[len(stored)-4:])
	}
	if len(stored) != maxUserAgent-1 {
		t.Errorf("stored %d bytes; want %d, the last whole character", len(stored), maxUserAgent-1)
	}
}

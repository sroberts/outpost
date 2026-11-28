package auth

import (
	"errors"
	"testing"
)

// What CreateGrant won't make is ErrInvalidGrant, so the token endpoint can
// tell the client's problem from the server's.
func TestGrant_RefusalsAreInvalidGrant(t *testing.T) {
	s, _ := grantStore(t)
	if err := s.SetDisabled("bob", true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, user, app, client string }{
		{"a disabled user", "bob", "sous", claudeClient},
		{"no such user", "carol", "sous", claudeClient},
		{"no app", "alice", "", claudeClient},
		{"no client", "alice", "sous", ""},
	} {
		if _, _, _, err := s.CreateGrant(tc.user, tc.app, tc.client, "x"); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: %v; want ErrInvalidGrant", tc.name, err)
		}
	}
	s.Close()
	if _, _, _, err := s.CreateGrant("alice", "sous", claudeClient, "x"); err == nil || errors.Is(err, ErrInvalidGrant) {
		t.Errorf("a closed database: %v; want an error that isn't ErrInvalidGrant", err)
	}
}

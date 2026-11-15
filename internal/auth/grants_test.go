package auth

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudeClient = "https://claude.ai/oauth/claude-client-metadata"

func grantStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, c := newTestStore(t)
	for _, u := range []string{"alice", "bob"} {
		if err := s.AddUser(User{Username: u}, ""); err != nil {
			t.Fatal(err)
		}
	}
	return s, c
}

// A grant's access token is an internet token for its app alone, lasting an
// hour, and it stays out of the token list, which shows the grant instead.
func TestGrant_IssuesANarrowShortAccessToken(t *testing.T) {
	s, c := grantStore(t)
	access, refresh, g, err := s.CreateGrant("alice", "sous", claudeClient, "Claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(access, AccessTokenPrefix) || !strings.HasPrefix(refresh, RefreshTokenPrefix) {
		t.Errorf("access %q refresh %q: wrong prefixes", access[:5], refresh[:5])
	}
	if g.Username != "alice" || g.App != "sous" || g.ClientID != claudeClient || g.ClientName != "Claude" ||
		!g.ExpiresAt.Equal(c.now().Add(GrantIdleTTL)) {
		t.Errorf("grant = %+v", g)
	}
	tok, u, err := s.LookupAccessToken(access)
	if err != nil || u.Username != "alice" || !tok.Internet || len(tok.Apps) != 1 || tok.Apps[0] != "sous" ||
		!tok.ExpiresAt.Equal(c.now().Add(GrantAccessTTL)) {
		t.Errorf("access token = %+v, %v", tok, err)
	}
	if _, _, err := s.LookupAccessToken(refresh); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("the refresh token works as an access token: %v", err)
	}
	if list, err := s.ListAccessTokens(""); err != nil || len(list) != 0 {
		t.Errorf("token list = %+v, %v; want the grant's token left out", list, err)
	}
	if list, err := s.ListGrants("alice"); err != nil || len(list) != 1 || list[0].ID != g.ID {
		t.Errorf("grants = %+v, %v", list, err)
	}
	if list, err := s.ListGrants("bob"); err != nil || len(list) != 0 {
		t.Errorf("bob's grants = %+v, %v", list, err)
	}

	c.advance(GrantAccessTTL)
	if _, _, err := s.LookupAccessToken(access); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("an hour on, the access token = %v", err)
	}
}

// Each refresh token works once. The new pair works, the old access token
// keeps working until it expires, and the grant's life is extended.
func TestGrant_RefreshRotates(t *testing.T) {
	s, c := grantStore(t)
	access, refresh, _, err := s.CreateGrant("alice", "sous", claudeClient, "Claude")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(30 * 24 * time.Hour)
	access2, refresh2, g, err := s.RefreshGrant(refresh, claudeClient, "sous")
	if err != nil {
		t.Fatal(err)
	}
	if access2 == access || refresh2 == refresh || !g.RefreshedAt.Equal(c.now()) || !g.ExpiresAt.Equal(c.now().Add(GrantIdleTTL)) {
		t.Errorf("after refresh: %+v", g)
	}
	if _, _, err := s.LookupAccessToken(access2); err != nil {
		t.Errorf("new access token: %v", err)
	}
	// And again, with the new one.
	if _, _, _, err := s.RefreshGrant(refresh2, claudeClient, "sous"); err != nil {
		t.Errorf("second refresh: %v", err)
	}
}

// Presenting a refresh token that was already exchanged means two parties
// have it: the grant and every token it issued end.
func TestGrant_ReusedRefreshTokenRevokesTheGrant(t *testing.T) {
	s, _ := grantStore(t)
	_, refresh, g, err := s.CreateGrant("alice", "sous", claudeClient, "Claude")
	if err != nil {
		t.Fatal(err)
	}
	access2, refresh2, _, err := s.RefreshGrant(refresh, claudeClient, "sous")
	if err != nil {
		t.Fatal(err)
	}
	_, _, got, err := s.RefreshGrant(refresh, claudeClient, "sous")
	if !errors.Is(err, ErrRefreshReused) || got.ID != g.ID {
		t.Fatalf("reuse = %+v, %v", got, err)
	}
	if _, _, err := s.LookupAccessToken(access2); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Error("the grant's access token outlived it")
	}
	if _, _, _, err := s.RefreshGrant(refresh2, claudeClient, "sous"); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("the current refresh token after a reuse = %v", err)
	}
	if list, _ := s.ListGrants(""); len(list) != 0 {
		t.Errorf("grants = %+v", list)
	}
}

// A refresh token presented by another client or for another app, after the
// grant lapsed, or for a disabled user, is refused. The first two leave the
// grant alone, since the token wasn't replayed.
func TestGrant_RefreshRefusals(t *testing.T) {
	s, c := grantStore(t)
	_, refresh, _, err := s.CreateGrant("alice", "sous", claudeClient, "Claude")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, token, client, app string }{
		{"another client", refresh, "https://evil.example/client", "sous"},
		{"another app", refresh, claudeClient, "yarr"},
		{"an unknown token", RefreshTokenPrefix + "nope", claudeClient, "sous"},
		{"an access token's shape", AccessTokenPrefix + "nope", claudeClient, "sous"},
	} {
		if _, _, _, err := s.RefreshGrant(tc.token, tc.client, tc.app); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	if _, _, _, err := s.RefreshGrant(refresh, claudeClient, "sous"); err != nil {
		t.Fatalf("refused attempts spoiled the grant: %v", err)
	}

	_, refresh, _, err = s.CreateGrant("bob", "sous", claudeClient, "Claude")
	if err != nil {
		t.Fatal(err)
	}
	c.advance(GrantIdleTTL)
	if _, _, _, err := s.RefreshGrant(refresh, claudeClient, "sous"); !errors.Is(err, ErrInvalidGrant) {
		t.Errorf("a lapsed grant refreshed: %v", err)
	}
}

// Everything that shuts a user out ends their grants too, so a grant can't
// mint tokens for someone who has been shut out.
func TestGrant_EndsWithTheUsersAccess(t *testing.T) {
	for _, tc := range []struct {
		name string
		shut func(*Store) error
	}{
		{"logout", func(s *Store) error { _, _, err := s.RevokeUserAccess("alice"); return err }},
		{"disable", func(s *Store) error { return s.SetDisabled("alice", true) }},
		{"password", func(s *Store) error { return s.SetPassword("alice", "a new long password") }},
		{"remove", func(s *Store) error { return s.RemoveUser("alice") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := grantStore(t)
			access, refresh, _, err := s.CreateGrant("alice", "sous", claudeClient, "Claude")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := s.CreateGrant("bob", "sous", claudeClient, "Claude"); err != nil {
				t.Fatal(err)
			}
			if err := tc.shut(s); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.LookupAccessToken(access); !errors.Is(err, ErrNoSuchAccessToken) {
				t.Error("the access token outlived the user's access")
			}
			if _, _, _, err := s.RefreshGrant(refresh, claudeClient, "sous"); err == nil {
				t.Error("the grant refreshed after the user's access ended")
			}
			list, _ := s.ListGrants("")
			if len(list) != 1 || list[0].Username != "bob" {
				t.Errorf("grants left = %+v; want only bob's", list)
			}
		})
	}
}

func TestGrant_RevokeAndRefusals(t *testing.T) {
	s, c := grantStore(t)
	access, _, g, err := s.CreateGrant("alice", "sous", claudeClient, "Claude\x1b[31m  ")
	if err != nil {
		t.Fatal(err)
	}
	if g.ClientName != "Claude[31m" {
		t.Errorf("client name = %q; want control characters out and trimmed", g.ClientName)
	}
	got, err := s.RevokeGrant(g.ID)
	if err != nil || got.ID != g.ID {
		t.Fatalf("revoke = %+v, %v", got, err)
	}
	if _, _, err := s.LookupAccessToken(access); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Error("the access token outlived its grant")
	}
	if _, err := s.RevokeGrant(g.ID); !errors.Is(err, ErrNoSuchGrant) {
		t.Errorf("revoking twice = %v", err)
	}

	if err := s.SetDisabled("bob", true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, user, app, client string }{
		{"a disabled user", "bob", "sous", claudeClient},
		{"no such user", "carol", "sous", claudeClient},
		{"no app", "alice", "", claudeClient},
		{"no client", "alice", "sous", ""},
	} {
		if _, _, _, err := s.CreateGrant(tc.user, tc.app, tc.client, "x"); err == nil {
			t.Errorf("%s: granted", tc.name)
		}
	}

	// Making a grant sweeps lapsed ones.
	if _, _, _, err := s.CreateGrant("alice", "sous", claudeClient, "old"); err != nil {
		t.Fatal(err)
	}
	c.advance(GrantIdleTTL)
	if list, _ := s.ListGrants(""); len(list) != 1 || !list[0].Lapsed(c.now()) {
		t.Errorf("a lapsed grant should stay listed until swept: %+v", list)
	}
	if _, _, _, err := s.CreateGrant("alice", "sous", claudeClient, "new"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListGrants(""); len(list) != 1 || list[0].ClientName != "new" {
		t.Errorf("after a new grant = %+v", list)
	}
}

// A database from before grants gains the table and the column on open, and
// its tokens read as made by hand.
func TestGrant_MigratesADatabaseWithoutGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Params = testParams
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "cron", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX access_tokens_grant`, `ALTER TABLE access_tokens DROP COLUMN grant_id`, `DROP TABLE oauth_grants`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if list, err := s.ListAccessTokens(""); err != nil || len(list) != 1 {
		t.Errorf("tokens after migrating = %+v, %v", list, err)
	}
	if _, _, err := s.LookupAccessToken(secret); err != nil {
		t.Errorf("an old token after migrating: %v", err)
	}
	if _, _, _, err := s.CreateGrant("alice", "sous", claudeClient, "Claude"); err != nil {
		t.Errorf("granting after migrating: %v", err)
	}
}

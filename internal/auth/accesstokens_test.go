package auth

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// schemaV1 is auth.db as it was before access tokens existed, copied rather
// than derived from the current schema, so upgrading stays tested against what
// is really on disk on a machine that has been running for a while.
const schemaV1 = `
CREATE TABLE users (
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
CREATE TABLE user_groups (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name    TEXT NOT NULL,
    PRIMARY KEY (user_id, name)
);
CREATE TABLE sessions (
    token_hash   TEXT PRIMARY KEY,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_hash  TEXT NOT NULL DEFAULT '',
    app          TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    last_seen_at INTEGER NOT NULL,
    user_agent   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE INDEX sessions_parent ON sessions(parent_hash);
CREATE TABLE signing_keys (
    kid        TEXT PRIMARY KEY,
    private    BLOB NOT NULL,
    created_at INTEGER NOT NULL,
    retired_at INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
INSERT INTO meta (key, value) VALUES ('schema_version', '1');
INSERT INTO users (id, username, display_name, password_hash, is_admin, created_at)
VALUES (7, 'alice', 'Alice', 'not-a-real-hash', 1, 1700000000);
INSERT INTO user_groups (user_id, name) VALUES (7, 'family');
INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen_at)
VALUES ('abc', 7, 1700000000, 4000000000, 1700000000);
INSERT INTO signing_keys (kid, private, created_at) VALUES ('k1', x'00', 1700000000);
`

func TestOpen_UpgradesADatabaseFromBeforeAccessTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open the old database: %v", err)
	}
	defer func() { _ = s.Close() }() // reopened below, which is what's under test

	// Everything that was there still is.
	u, err := s.GetUser("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Admin || u.DisplayName != "Alice" || len(u.Groups) != 1 || u.Groups[0] != "family" || u.Sessions != 1 {
		t.Errorf("alice after the upgrade = %+v", u)
	}
	// Additive tables need no version change, and a daemon rolled back to the
	// old binary would ignore them.
	if v, err := s.SchemaVersion(); err != nil || v != "1" {
		t.Errorf("schema version = %q, %v", v, err)
	}

	// Tokens work on it, cascades included.
	secret, tok, err := s.CreateAccessToken("alice", "cron", []string{"yarr"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	got, _, err := s.LookupAccessToken(secret)
	if err != nil || got.ID != tok.ID || !got.Reaches("yarr") || got.Reaches("wiki") {
		t.Errorf("after a reopen: %+v, %v", got, err)
	}
	if err := s.AddUser(User{Username: "bob", Admin: true}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("token of a removed user: %v", err)
	}
	assertTokenRows(t, s, 0)
}

// assertTokenRows checks how many rows remain, scope rows included, so nothing
// is left orphaned behind a revoked token.
func assertTokenRows(t *testing.T, s *Store, want int) {
	t.Helper()
	var n, apps int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM access_tokens`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM access_token_apps`).Scan(&apps); err != nil {
		t.Fatal(err)
	}
	if n != want || (want == 0 && apps != 0) {
		t.Errorf("%d token rows and %d scope rows; want %d tokens", n, apps, want)
	}
}

func TestAccessToken_OnlyAHashIsStored(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "cron", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, AccessTokenPrefix) {
		t.Errorf("secret %q lacks the prefix", secret)
	}
	// 32 bytes of base64url without padding.
	if n := len(strings.TrimPrefix(secret, AccessTokenPrefix)); n != 43 {
		t.Errorf("secret body is %d characters; want 43 for 256 bits", n)
	}
	var hash, name string
	if err := s.db.QueryRow(`SELECT token_hash, name FROM access_tokens`).Scan(&hash, &name); err != nil {
		t.Fatal(err)
	}
	if hash != HashToken(secret) {
		t.Errorf("stored %q; want the SHA-256 of the secret", hash)
	}
	if strings.Contains(hash, secret) || strings.Contains(name, secret) {
		t.Error("the secret itself is in the database")
	}
	other, _, err := s.CreateAccessToken("alice", "cron", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if other == secret {
		t.Error("two tokens came out the same")
	}
}

func TestAccessToken_LookupSeesTheUserAsTheyAreNow(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice", Groups: []string{"family"}}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateGroups("alice", []string{"kids"}, []string{"family"}); err != nil {
		t.Fatal(err)
	}
	_, u, err := s.LookupAccessToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Groups) != 1 || u.Groups[0] != "kids" {
		t.Errorf("groups = %v; want the current ones", u.Groups)
	}
}

func TestAccessToken_ScopeIsCheckedAndNeverWidens(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, apps := range [][]string{{""}, {"yarr", ""}, {"yarr,wiki"}, {"ya rr"}, {"ya\x00rr"}} {
		if _, _, err := s.CreateAccessToken("alice", "", apps, 0); err == nil {
			t.Errorf("scope %q was accepted", apps)
		}
	}
	assertTokenRows(t, s, 0)

	secret, tok, err := s.CreateAccessToken("alice", "", []string{"wiki", "yarr", "wiki"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok.Apps) != 2 || tok.Apps[0] != "wiki" || tok.Apps[1] != "yarr" {
		t.Errorf("apps = %v", tok.Apps)
	}
	got, _, err := s.LookupAccessToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reaches("wiki") || !got.Reaches("yarr") || got.Reaches("mail") {
		t.Errorf("scope wrong: %+v", got)
	}
	if !(AccessToken{}).Reaches("anything") {
		t.Error("a token with no scope should reach every app")
	}
}

func TestAccessToken_Refusals(t *testing.T) {
	s, _ := newTestStore(t)
	if _, _, err := s.CreateAccessToken("ghost", "", nil, 0); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("unknown user: %v", err)
	}
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccessToken("alice", "bad\nname", nil, 0); err == nil {
		t.Error("a name with a newline was accepted")
	}
	if _, _, err := s.CreateAccessToken("alice", "", nil, -time.Hour); err == nil {
		t.Error("an expiry in the past was accepted")
	}
	if err := s.SetDisabled("alice", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccessToken("alice", "", nil, 0); err == nil {
		t.Error("a token was made for a disabled user")
	}
	for _, secret := range []string{"", AccessTokenPrefix, AccessTokenPrefix + "nope", "nope"} {
		if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
			t.Errorf("lookup %q: %v", secret, err)
		}
	}
}

func TestAccessToken_Expires(t *testing.T) {
	s, clk := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, tok, err := s.CreateAccessToken("alice", "", nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	never, _, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Expired(clk.now()) {
		t.Error("expired at birth")
	}
	clk.advance(24*time.Hour - time.Second)
	if _, _, err := s.LookupAccessToken(secret); err != nil {
		t.Errorf("just before expiry: %v", err)
	}
	clk.advance(time.Second)
	if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("at expiry: %v", err)
	}
	if !tok.Expired(clk.now()) {
		t.Error("Expired disagrees with the lookup")
	}
	clk.advance(10 * 365 * 24 * time.Hour)
	if _, _, err := s.LookupAccessToken(never); err != nil {
		t.Errorf("a token with no expiry expired: %v", err)
	}

	// An expired token stays listed, so someone can see why a script stopped,
	// but isn't counted as something to end.
	list, err := s.ListAccessTokens("alice")
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, %v", list, err)
	}
	if n, err := s.CountAccessTokens("alice"); err != nil || n != 1 {
		t.Errorf("count = %d, %v", n, err)
	}
}

// Everything that shuts a person out ends their tokens, and enabling them again
// brings nothing back.
func TestAccessToken_EndsWithItsUser(t *testing.T) {
	cases := []struct {
		name string
		end  func(*Store) error
	}{
		{"disable", func(s *Store) error { return s.SetDisabled("alice", true) }},
		{"remove", func(s *Store) error { return s.RemoveUser("alice") }},
		{"logout", func(s *Store) error { _, err := s.RevokeUserSessions("alice"); return err }},
		{"access", func(s *Store) error { _, _, err := s.RevokeUserAccess("alice"); return err }},
		{"password", func(s *Store) error { return s.SetPassword("alice", "a whole new password") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestStore(t)
			if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
				t.Fatal(err)
			}
			if err := s.AddUser(User{Username: "bob"}, ""); err != nil {
				t.Fatal(err)
			}
			secret, _, err := s.CreateAccessToken("alice", "", []string{"yarr"}, 0)
			if err != nil {
				t.Fatal(err)
			}
			bobs, _, err := s.CreateAccessToken("bob", "", nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.end(s); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
				t.Errorf("token still works: %v", err)
			}
			if _, _, err := s.LookupAccessToken(bobs); err != nil {
				t.Errorf("someone else's token was ended: %v", err)
			}
			var scope int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM access_token_apps`).Scan(&scope); err != nil {
				t.Fatal(err)
			}
			if scope != 0 {
				t.Errorf("%d scope rows left behind", scope)
			}
			if tc.name == "disable" {
				if err := s.SetDisabled("alice", false); err != nil {
					t.Fatal(err)
				}
				if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
					t.Errorf("enabling brought the token back: %v", err)
				}
			}
		})
	}
}

// A row that somehow survives is still no good while its user is disabled.
func TestAccessToken_DisabledUserIsCheckedAtLookup(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'alice'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("lookup = %v", err)
	}
}

// Ending one browser sign-in is about that device; a script's token is not on
// it.
func TestAccessToken_SurvivesEndingOneSignIn(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, end := range []func(portal string) error{
		func(p string) error { return s.RevokeSession(p) },
		func(p string) error { return s.RevokeFamily(p) },
		func(p string) error { return s.EndSignIn("alice", HashToken(p)) },
		func(string) error { _, err := s.EndOtherSignIns("alice", ""); return err },
	} {
		portal, err := s.CreateSession("alice", "", "", "ua")
		if err != nil {
			t.Fatal(err)
		}
		if err := end(portal); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.LookupAccessToken(secret); err != nil {
			t.Errorf("token ended with a sign-in: %v", err)
		}
	}
}

func TestRevokeUserAccess_Counts(t *testing.T) {
	s, clk := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSession("alice", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccessToken("alice", "", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	clk.advance(2 * time.Hour)
	if _, _, err := s.CreateAccessToken("alice", "", nil, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAccessToken("alice", "", nil, 0); err != nil {
		t.Fatal(err)
	}
	sessions, tokens, err := s.RevokeUserAccess("alice")
	if err != nil {
		t.Fatal(err)
	}
	if sessions != 1 || tokens != 2 {
		t.Errorf("reported %d sessions and %d tokens; want 1 and 2, the expired one not counted", sessions, tokens)
	}
	assertTokenRows(t, s, 0)
	if _, _, err := s.RevokeUserAccess("ghost"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("unknown user: %v", err)
	}
}

func TestAccessToken_TouchIsPeriodic(t *testing.T) {
	s, clk := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, _, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	use := func() time.Time {
		t.Helper()
		tok, _, err := s.LookupAccessToken(secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.TouchAccessToken(tok); err != nil {
			t.Fatal(err)
		}
		tok, _, err = s.LookupAccessToken(secret)
		if err != nil {
			t.Fatal(err)
		}
		return tok.LastUsedAt
	}
	first := use()
	if !first.Equal(clk.now()) {
		t.Errorf("first use recorded %v; want %v", first, clk.now())
	}
	clk.advance(AccessTokenTouchEvery - time.Second)
	if got := use(); !got.Equal(first) {
		t.Errorf("moved after %v: %v", AccessTokenTouchEvery-time.Second, got)
	}
	clk.advance(time.Second)
	if got := use(); !got.Equal(clk.now()) {
		t.Errorf("didn't move when due: %v", got)
	}
}

func TestAccessToken_ListAndRevoke(t *testing.T) {
	s, _ := newTestStore(t)
	for _, name := range []string{"alice", "bob"} {
		if err := s.AddUser(User{Username: name}, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, a, err := s.CreateAccessToken("alice", "one", []string{"yarr"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	secret, b, err := s.CreateAccessToken("bob", "two", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAccessTokens("")
	if err != nil || len(all) != 2 || all[0].ID != a.ID || all[1].ID != b.ID {
		t.Fatalf("list all = %+v, %v", all, err)
	}
	if len(all[0].Apps) != 1 || all[0].Name != "one" || all[0].Username != "alice" {
		t.Errorf("first = %+v", all[0])
	}
	mine, err := s.ListAccessTokens("bob")
	if err != nil || len(mine) != 1 || mine[0].ID != b.ID {
		t.Errorf("list bob = %+v, %v", mine, err)
	}
	if _, err := s.ListAccessTokens("ghost"); !errors.Is(err, ErrNoSuchUser) {
		t.Errorf("list ghost: %v", err)
	}

	got, err := s.RevokeAccessToken(b.ID)
	if err != nil || got.Username != "bob" {
		t.Fatalf("revoke = %+v, %v", got, err)
	}
	if _, _, err := s.LookupAccessToken(secret); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("revoked token: %v", err)
	}
	if _, err := s.RevokeAccessToken(b.ID); !errors.Is(err, ErrNoSuchAccessToken) {
		t.Errorf("second revoke: %v", err)
	}

	// The id of the newest token, just revoked, isn't handed out again, or a
	// stale `token revoke` could end someone else's.
	_, next, err := s.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID <= b.ID {
		t.Errorf("id %d reused after %d was revoked", next.ID, b.ID)
	}
}

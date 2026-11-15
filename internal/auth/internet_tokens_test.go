package auth

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestInternetAccessToken_IsMarkedAndNarrow(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	secret, tok, err := s.CreateInternetAccessToken("alice", "claude-phone", "sous", 90*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !tok.Internet || len(tok.Apps) != 1 || tok.Apps[0] != "sous" || tok.ExpiresAt.IsZero() {
		t.Errorf("created = %+v; want internet, sous only, expiring", tok)
	}
	got, _, err := s.LookupAccessToken(secret)
	if err != nil || !got.Internet {
		t.Errorf("lookup = %+v, %v; want it marked internet", got, err)
	}

	// An ordinary token is not.
	plain, _, err := s.CreateAccessToken("alice", "cron", []string{"sous"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := s.LookupAccessToken(plain); err != nil || got.Internet {
		t.Errorf("ordinary lookup = %+v, %v; want tailnet-only", got, err)
	}

	list, err := s.ListAccessTokens("alice")
	if err != nil || len(list) != 2 || !list[0].Internet || list[1].Internet {
		t.Errorf("list = %+v, %v; want the first internet and the second not", list, err)
	}
}

// What the internet can reach with a leaked token is the risk, so the store
// refuses a wide or everlasting one whatever the caller checked.
func TestInternetAccessToken_Refusals(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, app string
		ttl       time.Duration
	}{
		{"no app", "", 24 * time.Hour},
		{"no expiry", "sous", 0},
		{"past expiry", "sous", -time.Hour},
		{"bad app name", "sous,yarr", 24 * time.Hour},
	} {
		if _, _, err := s.CreateInternetAccessToken("alice", "", c.app, c.ttl); err == nil {
			t.Errorf("%s: created", c.name)
		}
	}
	if list, _ := s.ListAccessTokens("alice"); len(list) != 0 {
		t.Errorf("a refusal left %d token(s) behind", len(list))
	}
}

// A database whose tokens predate the flag gains it on open, and every token
// already there stays tailnet-only.
func TestInternetAccessToken_MigratesAnOlderDatabase(t *testing.T) {
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
    last_login_at INTEGER NOT NULL DEFAULT 0,
    tailnet_login TEXT NOT NULL DEFAULT ''
);
CREATE TABLE access_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash   TEXT NOT NULL UNIQUE,
    user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL DEFAULT 0,
    last_used_at INTEGER NOT NULL DEFAULT 0
);
INSERT INTO users (username, created_at) VALUES ('old', 1);
INSERT INTO access_tokens (token_hash, user_id, name, created_at) VALUES (?, 1, 'script', 1);`,
		HashToken(AccessTokenPrefix+"old")); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := s.LookupAccessToken(AccessTokenPrefix + "old")
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	if tok.Internet {
		t.Error("a token from before the flag came out marked internet")
	}
	var used int64
	if err := s.db.QueryRow(`SELECT internet_used_at FROM access_tokens WHERE id = ?`, tok.ID).Scan(&used); err != nil || used != 0 {
		t.Errorf("internet_used_at = %d, %v; want the column, unset", used, err)
	}
	// Opening again finds the columns there and leaves them.
	s.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	again.Close()
}

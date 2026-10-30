package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/ipc"
)

func TestCLI_TokenCreateListRevoke(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}

	// Stdout is the secret and nothing else, so it can be captured by a script.
	out, errOut, err := runCLI(t, home, "token", "create", "alice", "--name", "cron", "--app", "yarr", "--app", "wiki", "--expires", "90d")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(out)
	if strings.Count(out, "\n") != 1 || !strings.HasPrefix(secret, auth.AccessTokenPrefix) {
		t.Errorf("stdout = %q; want only the secret", out)
	}
	if strings.Contains(errOut, secret) {
		t.Errorf("the secret is on stderr too: %q", errOut)
	}
	for _, want := range []string{"for alice", "wiki, yarr", "only time", "Warning: no service is called wiki, yarr"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %q", want, errOut)
		}
	}
	tok, _, err := as.LookupAccessToken(secret)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Name != "cron" || !tok.Reaches("wiki") || tok.Reaches("mail") || tok.ExpiresAt.IsZero() {
		t.Errorf("token = %+v", tok)
	}

	// A flag after the name and one before both count, as elsewhere.
	if out, _, err = runCLI(t, home, "token", "create", "--name", "second", "alice"); err != nil {
		t.Fatal(err)
	}
	second := strings.TrimSpace(out)

	out, _, err = runCLI(t, home, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "LAST USED", "alice", "cron", "wiki,yarr", "second", "all", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, secret) || strings.Contains(out, second) || strings.Contains(out, auth.AccessTokenPrefix) {
		t.Errorf("list shows a secret:\n%s", out)
	}
	if out, _, err = runCLI(t, home, "token", "list", "alice"); err != nil || !strings.Contains(out, "cron") {
		t.Errorf("list alice = %q, %v", out, err)
	}
	if _, _, err = runCLI(t, home, "token", "list", "ghost"); err == nil {
		t.Error("list of an unknown user succeeded")
	}

	id := strconv.FormatInt(tok.ID, 10)
	if out, _, err = runCLI(t, home, "token", "revoke", id); err != nil || !strings.Contains(out, "revoked access token "+id) {
		t.Errorf("revoke = %q, %v", out, err)
	}
	if _, _, err := as.LookupAccessToken(secret); err == nil {
		t.Error("the token works after a revoke")
	}
	if _, _, err = runCLI(t, home, "token", "revoke", id); err == nil {
		t.Error("revoking twice succeeded")
	}
}

func TestCLI_TokenUsageErrors(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"token"},
		{"token", "bogus"},
		{"token", "create"},
		{"token", "create", "alice", "bob"},
		{"token", "create", "ghost"},
		{"token", "create", "alice", "--expires", "soon"},
		{"token", "create", "alice", "--expires", "-1d"},
		{"token", "create", "alice", "--app", ""},
		{"token", "list", "alice", "bob"},
		{"token", "revoke"},
		{"token", "revoke", "abc"},
		{"token", "revoke", "0"},
		{"token", "revoke", "1", "2"},
	} {
		if _, _, err := runCLI(t, home, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
	// A refused request has nothing to show for it.
	if list, err := as.ListAccessTokens(""); err != nil || len(list) != 0 {
		t.Errorf("tokens = %+v, %v", list, err)
	}
	out, _, err := runCLI(t, home, "token", "list")
	if err != nil || !strings.Contains(out, "No access tokens") {
		t.Errorf("empty list = %q, %v", out, err)
	}
	if out, _, err := runCLI(t, home, "token", "help"); err != nil || !strings.Contains(out, "token create") {
		t.Errorf("help = %q, %v", out, err)
	}
}

func TestCLI_TokenNeedsADaemon(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "token", "list"); err == nil || !strings.Contains(strings.ToLower(err.Error()), "daemon") {
		t.Errorf("no daemon: %v", err)
	}
}

func TestParseExpiry(t *testing.T) {
	day := 24 * time.Hour
	for in, want := range map[string]time.Duration{
		"90d":    90 * day,
		"1d":     day,
		"12w":    84 * day,
		"24h":    24 * time.Hour,
		"90m":    90 * time.Minute,
		"36500d": 36500 * day,
		"5214w":  5214 * 7 * day,
	} {
		got, err := parseExpiry(in)
		if err != nil || got != want {
			t.Errorf("%q = %v, %v; want %v", in, got, err, want)
		}
	}
	// Past the limit is refused rather than wrapping round into the past.
	for _, in := range []string{"", "d", "w", "0d", "-1d", "0", "0h", "-1h", "soon", "1.5d", "1y", "36501d", "5215w",
		"9999999999999999d", "9999999999999999w"} {
		if got, err := parseExpiry(in); err == nil {
			t.Errorf("%q = %v; want an error", in, got)
		}
	}
}

func TestWriteTokenTable_SaysWhenExpired(t *testing.T) {
	var b strings.Builder
	soon := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	if err := writeTokenTable(&b, []ipc.TokenInfo{
		{ID: 1, Username: "alice", Expired: true, ExpiresAt: soon},
		{ID: 2, Username: "bob", Name: "n", Apps: []string{"a", "b"}, ExpiresAt: soon, LastUsedAt: soon},
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"expired", "a,b", soon.Local().Format("2006-01-02 15:04")} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
}

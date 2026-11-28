package main

import (
	"strconv"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// token list shows OAuth clients in a table of their own, without their
// hourly tokens, and revoke-client disconnects one.
func TestCLI_TokenListAndRevokeClients(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, home, "token", "list")
	if err != nil || strings.Contains(out, "Connected clients") {
		t.Errorf("with no clients = %q, %v", out, err)
	}

	access, refresh, g, err := as.CreateGrant("alice", "sous", "https://claude.ai/oauth/claude-client-metadata", "Claude")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, home, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"No access tokens", "Connected clients", "Claude (claude.ai)", "sous", "LAST RENEWED", "never"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, access) || strings.Contains(out, refresh) || strings.Contains(out, auth.RefreshTokenPrefix) {
		t.Errorf("list shows a secret:\n%s", out)
	}

	id := strconv.FormatInt(g.ID, 10)
	out, _, err = runCLI(t, home, "token", "revoke-client", id)
	if err != nil || !strings.Contains(out, "disconnected Claude (claude.ai) for alice on sous (client "+id+")") {
		t.Errorf("revoke-client = %q, %v", out, err)
	}
	if _, _, err := as.LookupAccessToken(access); err == nil {
		t.Error("the client's access token outlived it")
	}
	for _, args := range [][]string{
		{"token", "revoke-client", id},
		{"token", "revoke-client", "nope"},
		{"token", "revoke-client"},
	} {
		if _, _, err := runCLI(t, home, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

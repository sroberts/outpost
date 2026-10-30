package ipc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/gateway"
)

func TestToken_CreateListRevoke(t *testing.T) {
	c, as, logs := authDaemon(t, true)
	if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}

	secret, info, warning, err := c.TokenCreate(TokenRequest{
		Username: "alice", Name: "cron", Apps: []string{"yarr"}, ExpiresIn: 90 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, auth.AccessTokenPrefix) {
		t.Errorf("secret = %q", secret)
	}
	if info.Username != "alice" || info.Name != "cron" || len(info.Apps) != 1 || info.ExpiresAt.IsZero() {
		t.Errorf("info = %+v", info)
	}
	if d := time.Until(info.ExpiresAt); d < 89*24*time.Hour || d > 91*24*time.Hour {
		t.Errorf("expires in %v", d)
	}
	if !strings.Contains(warning, "yarr") {
		t.Errorf("a scope naming no service should say so: %q", warning)
	}
	if _, _, err := as.LookupAccessToken(secret); err != nil {
		t.Errorf("the secret doesn't work: %v", err)
	}

	// Nothing after creation can say what it was.
	list, err := c.TokenList("")
	if err != nil || len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("list = %+v, %v", list, err)
	}
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), auth.HashToken(secret)) {
		t.Errorf("list carries the secret or its hash: %s", raw)
	}
	if list, err := c.TokenList("alice"); err != nil || len(list) != 1 {
		t.Errorf("list alice = %+v, %v", list, err)
	}
	if _, err := c.TokenList("ghost"); err == nil {
		t.Error("listing an unknown user succeeded")
	}

	msg, err := c.TokenRevoke(info.ID)
	if err != nil || !strings.Contains(msg, "revoked") {
		t.Errorf("revoke = %q, %v", msg, err)
	}
	if _, _, err := as.LookupAccessToken(secret); err == nil {
		t.Error("the secret works after a revoke")
	}
	if _, err := c.TokenRevoke(info.ID); err == nil {
		t.Error("revoking twice succeeded")
	}

	// The feed records both, with an id and no secret.
	events, err := c.Events(gateway.ActivityService, 10)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, e := range events {
		kinds[e.Kind] = true
		if strings.Contains(e.Message, secret) {
			t.Errorf("the secret reached the feed: %q", e.Message)
		}
	}
	if !kinds[gateway.EventTokenCreate] || !kinds[gateway.EventTokenRevoke] {
		t.Errorf("feed = %+v", events)
	}
	if strings.Contains(logs.String(), secret) {
		t.Error("the secret reached the log")
	}
}

func TestToken_CreateRefusals(t *testing.T) {
	c, _, _ := authDaemon(t, true)
	if _, _, _, err := c.TokenCreate(TokenRequest{Username: "ghost"}); err == nil {
		t.Error("a token for nobody was created")
	}
	if _, _, _, err := c.TokenCreate(TokenRequest{}); err == nil {
		t.Error("a token with no user was created")
	}
	if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.TokenCreate(TokenRequest{Username: "alice", Apps: []string{""}}); err == nil {
		t.Error("an empty app name was accepted")
	}
	if secret, _, warning, err := c.TokenCreate(TokenRequest{Username: "alice"}); err != nil || secret == "" || warning != "" {
		t.Errorf("no scope, no warning: %q, %q, %v", secret, warning, err)
	}
}

func TestToken_WithoutAnAuthStore(t *testing.T) {
	c, _, _ := authDaemon(t, false)
	if _, _, _, err := c.TokenCreate(TokenRequest{Username: "alice"}); err == nil {
		t.Error("create without an auth store succeeded")
	}
	if _, err := c.TokenList(""); err == nil {
		t.Error("list without an auth store succeeded")
	}
}

// Each user command that ends a person's access says so in the feed, because
// the owner of a script otherwise finds out from its next 401.
func TestToken_UserCommandsRevokeAndSaySo(t *testing.T) {
	for name, end := range map[string]func(*Client) error{
		"disable": func(c *Client) error { _, err := c.UserDisable("alice"); return err },
		"logout":  func(c *Client) error { _, err := c.UserLogout("alice"); return err },
		"rm":      func(c *Client) error { _, err := c.UserRemove("alice"); return err },
		"passwd":  func(c *Client) error { _, err := c.UserPasswd("alice", testPassword); return err },
	} {
		t.Run(name, func(t *testing.T) {
			c, as, _ := authDaemon(t, true)
			if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
				t.Fatal(err)
			}
			secret, _, _, err := c.TokenCreate(TokenRequest{Username: "alice"})
			if err != nil {
				t.Fatal(err)
			}
			if err := end(c); err != nil {
				t.Fatal(err)
			}
			if _, _, err := as.LookupAccessToken(secret); err == nil {
				t.Error("the token still works")
			}
			events, err := c.Events(gateway.ActivityService, 10)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, e := range events {
				found = found || (e.Kind == gateway.EventTokenRevoke && strings.Contains(e.Message, "1 access token(s) for alice"))
			}
			if !found {
				t.Errorf("feed doesn't say so: %+v", events)
			}
		})
	}
}

func TestToken_LogoutReportsBoth(t *testing.T) {
	c, as, _ := authDaemon(t, true)
	if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := as.CreateSession("alice", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.TokenCreate(TokenRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	msg, err := c.UserLogout("alice")
	if err != nil || !strings.Contains(msg, "ended 1 session") || !strings.Contains(msg, "1 access token") {
		t.Errorf("logout = %q, %v", msg, err)
	}
}

// A request without the token field stays byte-identical on the wire, which
// matters to a CLI and daemon of different builds.
func TestToken_RequestFieldIsOptionalOnTheWire(t *testing.T) {
	b, err := json.Marshal(Request{Command: CmdList})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"command":"list"}` {
		t.Errorf("wire form = %s", b)
	}
}

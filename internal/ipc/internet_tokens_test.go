package ipc

import (
	"testing"
	"time"
)

func TestToken_InternetCreate(t *testing.T) {
	c, as, _ := authDaemon(t, true)
	if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	secret, info, _, err := c.TokenCreate(TokenRequest{
		Username: "alice", Apps: []string{"sous"}, ExpiresIn: 24 * time.Hour, Internet: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Internet {
		t.Errorf("info = %+v; want it marked internet", info)
	}
	if tok, _, err := as.LookupAccessToken(secret); err != nil || !tok.Internet {
		t.Errorf("stored = %+v, %v; want it marked internet", tok, err)
	}
	list, err := c.TokenList("alice")
	if err != nil || len(list) != 1 || !list[0].Internet {
		t.Errorf("list = %+v, %v", list, err)
	}
}

// The daemon checks what the CLI checks, so another client can't get round it,
// and two apps are refused rather than cut down to one.
func TestToken_InternetRefusals(t *testing.T) {
	c, as, _ := authDaemon(t, true)
	if _, err := c.UserAdd(UserRequest{Username: "alice"}); err != nil {
		t.Fatal(err)
	}
	day := 24 * time.Hour
	for name, r := range map[string]TokenRequest{
		"no app":    {Username: "alice", ExpiresIn: day, Internet: true},
		"two apps":  {Username: "alice", Apps: []string{"sous", "yarr"}, ExpiresIn: day, Internet: true},
		"no expiry": {Username: "alice", Apps: []string{"sous"}, Internet: true},
	} {
		if _, _, _, err := c.TokenCreate(r); err == nil {
			t.Errorf("%s: created", name)
		}
	}
	if list, _ := as.ListAccessTokens("alice"); len(list) != 0 {
		t.Errorf("refusals left %d token(s)", len(list))
	}
}

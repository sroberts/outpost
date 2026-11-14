package main

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

func TestCLI_TokenCreateInternet(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCLI(t, home, "token", "create", "alice", "--name", "claude-phone", "--app", "sous", "--internet", "--expires", "90d")
	if err != nil {
		t.Fatal(err)
	}
	secret := strings.TrimSpace(out)
	if strings.Count(out, "\n") != 1 || !strings.HasPrefix(secret, auth.AccessTokenPrefix) {
		t.Errorf("stdout = %q; want only the secret", out)
	}
	if !strings.Contains(errOut, "from the internet") {
		t.Errorf("stderr doesn't say it works from the internet: %q", errOut)
	}
	if tok, _, err := as.LookupAccessToken(secret); err != nil || !tok.Internet {
		t.Errorf("token = %+v, %v; want it marked internet", tok, err)
	}
	if _, _, err := runCLI(t, home, "token", "create", "alice", "--name", "cron"); err != nil {
		t.Fatal(err)
	}

	out, _, err = runCLI(t, home, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	var phone, cron string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "claude-phone"):
			phone = line
		case strings.Contains(line, "cron"):
			cron = line
		}
	}
	if !strings.Contains(out, "FROM") || !strings.Contains(phone, "internet") || !strings.Contains(cron, "tailnet") {
		t.Errorf("list doesn't say which tokens work from the internet:\n%s", out)
	}
}

func TestCLI_TokenCreateInternetRefusals(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--internet", "--expires", "90d"}, "exactly one --app"},
		{[]string{"--internet", "--app", "sous", "--app", "yarr", "--expires", "90d"}, "exactly one --app"},
		{[]string{"--internet", "--app", "sous"}, "needs --expires"},
	} {
		args := append([]string{"token", "create", "alice"}, c.args...)
		if _, _, err := runCLI(t, home, args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: err = %v; want %q", c.args, err, c.want)
		}
	}
	if list, _ := as.ListAccessTokens("alice"); len(list) != 0 {
		t.Errorf("refusals left %d token(s)", len(list))
	}
}

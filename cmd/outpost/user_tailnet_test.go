package main

import (
	"strings"
	"testing"
)

func TestUserCLI_Tailnet(t *testing.T) {
	home, as := userDaemon(t)
	for _, name := range []string{"alice", "bob"} {
		if _, _, err := runCLI(t, home, "user", "add", name, "--no-password"); err != nil {
			t.Fatal(err)
		}
	}

	out, _, err := runCLI(t, home, "user", "tailnet", "alice", "Alice@GitHub")
	if err != nil {
		t.Fatalf("user tailnet alice: %v", err)
	}
	if !strings.Contains(out, "linked tailnet login alice@github to alice") {
		t.Errorf("output = %q", out)
	}
	if u, _ := as.GetUser("alice"); u.TailnetLogin != "alice@github" {
		t.Errorf("alice's tailnet login = %q", u.TailnetLogin)
	}

	// One user per login, and the refusal names who has it.
	if _, _, err := runCLI(t, home, "user", "tailnet", "bob", "alice@github"); err == nil || !strings.Contains(err.Error(), "already linked to alice") {
		t.Errorf("linking alice's login to bob: err = %v", err)
	}

	if _, _, err := runCLI(t, home, "user", "tailnet", "alice", "off"); err != nil {
		t.Fatalf("user tailnet alice off: %v", err)
	}
	if u, _ := as.GetUser("alice"); u.TailnetLogin != "" {
		t.Errorf("after off, alice's tailnet login = %q", u.TailnetLogin)
	}

	for _, args := range [][]string{
		{"user", "tailnet", "alice"},
		{"user", "tailnet", "alice", "not-a-login"},
		{"user", "tailnet", "nobody", "x@github"},
		{"user", "tailnet", "alice", "a@github", "extra"},
	} {
		if _, _, err := runCLI(t, home, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

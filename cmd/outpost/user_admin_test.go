package main

import (
	"strings"
	"testing"
)

func TestUserCLI_Admin(t *testing.T) {
	home, as := userDaemon(t)
	withStdin(t, cliPassword+"\n")
	if _, _, err := runCLI(t, home, "user", "add", "alice", "--admin", "--password-stdin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, home, "user", "add", "bob", "--no-password"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, home, "user", "admin", "bob", "on")
	if err != nil {
		t.Fatalf("user admin bob on: %v", err)
	}
	if !strings.Contains(out, "bob is now an admin") {
		t.Errorf("output = %q", out)
	}
	if u, _ := as.GetUser("bob"); !u.Admin {
		t.Error("bob is not an admin")
	}
	if _, _, err := runCLI(t, home, "user", "admin", "alice", "off"); err != nil {
		t.Fatalf("user admin alice off: %v", err)
	}
	if u, _ := as.GetUser("alice"); u.Admin {
		t.Error("alice is still an admin")
	}

	for _, args := range [][]string{
		{"user", "admin", "bob"},
		{"user", "admin", "bob", "maybe"},
		{"user", "admin", "bob", "on", "extra"},
	} {
		if _, _, err := runCLI(t, home, args...); err == nil {
			t.Errorf("%v succeeded", args)
		}
	}
}

package ipc

import (
	"strings"
	"testing"
)

// The last-admin guard tells the operator to make someone else an admin;
// user.admin is how they do it.
func TestAuth_UserAdmin(t *testing.T) {
	c, as, _ := authDaemon(t, true)
	if _, err := c.UserAdd(UserRequest{Username: "alice", Admin: true, Password: testPassword}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserAdd(UserRequest{Username: "bob"}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.UserAdmin("alice", false); err == nil || !strings.Contains(err.Error(), "user admin <name> on") {
		t.Errorf("demoting the last admin: %v; want a refusal naming the command", err)
	}

	msg, err := c.UserAdmin("bob", true)
	if err != nil {
		t.Fatalf("promote bob: %v", err)
	}
	if !strings.Contains(msg, "bob is now an admin") {
		t.Errorf("promote message = %q", msg)
	}
	if u, _ := as.GetUser("bob"); !u.Admin {
		t.Error("bob is not an admin after user.admin on")
	}

	if _, err := c.UserAdmin("alice", false); err != nil {
		t.Errorf("demote alice with bob an admin: %v", err)
	}
	if u, _ := as.GetUser("alice"); u.Admin {
		t.Error("alice is still an admin after user.admin off")
	}
	if _, err := c.UserAdmin("ghost", true); err == nil {
		t.Error("promoting a missing user succeeded")
	}
}

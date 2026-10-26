package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// userDaemon serves the control socket for a fresh home, with an auth store,
// and returns the home and the store. The home is under os.TempDir rather than
// t.TempDir, whose paths are too long for a unix socket on darwin.
func userDaemon(t *testing.T) (string, *auth.Store) {
	t.Helper()
	home, err := os.MkdirTemp("", "op")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })

	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	as, err := auth.Open(filepath.Join(home, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { as.Close() })
	as.Params = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

	srv, err := ipc.Listen(filepath.Join(home, "outpost.sock"), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseAuth(as)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})
	return home, as
}

// withStdin feeds the CLI's password input from s, as a pipe would.
func withStdin(t *testing.T, s string) {
	t.Helper()
	prevIn, prevTTY := stdin, stdinIsTerminal
	stdin = strings.NewReader(s)
	stdinIsTerminal = func() bool { return false }
	t.Cleanup(func() { stdin, stdinIsTerminal = prevIn, prevTTY })
}

// withTerminal stands in for a terminal that answers the prompts in order,
// and records how many were asked.
func withTerminal(t *testing.T, answers ...string) *int {
	t.Helper()
	prevTTY, prevRead := stdinIsTerminal, readSecret
	asked := 0
	stdinIsTerminal = func() bool { return true }
	readSecret = func() (string, error) {
		if asked >= len(answers) {
			t.Fatal("prompted more often than expected")
		}
		asked++
		return answers[asked-1], nil
	}
	t.Cleanup(func() { stdinIsTerminal, readSecret = prevTTY, prevRead })
	return &asked
}

const cliPassword = "correct horse battery"

func TestUserCLI_AddAndPasswdFromStdin(t *testing.T) {
	home, as := userDaemon(t)

	withStdin(t, cliPassword+"\n")
	out, _, err := runCLI(t, home, "user", "add", "bob", "--password-stdin", "--group", "family", "--group", "kids",
		"--email", "bob@example.com")
	if err != nil {
		t.Fatalf("user add: %v", err)
	}
	if !strings.Contains(out, "added user bob") {
		t.Errorf("user add output: %q", out)
	}
	u, err := as.GetUser("bob")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(u.Groups, ",") != "family,kids" || u.Email != "bob@example.com" || u.Admin {
		t.Errorf("bob = %+v", u)
	}
	if _, err := as.Authenticate("bob", cliPassword); err != nil {
		t.Errorf("bob can't log in with the stdin password: %v", err)
	}

	// No trailing newline, as from printf '%s'.
	withStdin(t, "a brand new passphrase")
	if _, _, err := runCLI(t, home, "user", "passwd", "bob", "--password-stdin"); err != nil {
		t.Fatalf("user passwd: %v", err)
	}
	if _, err := as.Authenticate("bob", "a brand new passphrase"); err != nil {
		t.Errorf("new password doesn't work: %v", err)
	}
}

func TestUserCLI_AddPromptsTwiceOnTerminal(t *testing.T) {
	home, as := userDaemon(t)
	asked := withTerminal(t, cliPassword, cliPassword)

	if _, _, err := runCLI(t, home, "user", "add", "alice", "--admin"); err != nil {
		t.Fatalf("user add: %v", err)
	}
	if *asked != 2 {
		t.Errorf("prompted %d times; want 2", *asked)
	}
	if u, err := as.Authenticate("alice", cliPassword); err != nil || !u.Admin {
		t.Errorf("alice = %+v, %v", u, err)
	}
}

func TestUserCLI_BadPasswordRejectedBeforeDaemon(t *testing.T) {
	home, as := userDaemon(t)

	cases := map[string]func(){
		"mismatch": func() { withTerminal(t, cliPassword, cliPassword+"x") },
		"short":    func() { withTerminal(t, "short", "short") },
		"stdin":    func() { withStdin(t, "short\n") },
		"empty":    func() { withStdin(t, "") },
	}
	for name, setup := range cases {
		setup()
		args := []string{"user", "add", "alice"}
		if name == "stdin" || name == "empty" {
			args = append(args, "--password-stdin")
		}
		if _, _, err := runCLI(t, home, args...); err == nil {
			t.Errorf("%s: user add succeeded", name)
		}
		if _, err := as.GetUser("alice"); err == nil {
			t.Fatalf("%s: alice was created despite a rejected password", name)
		}
	}

	// A short password on the first prompt stops before the second.
	asked := withTerminal(t, "short", "short")
	_, _, err := runCLI(t, home, "user", "add", "alice")
	if err == nil || !strings.Contains(err.Error(), "too short") {
		t.Errorf("short password: %v", err)
	}
	if *asked != 1 {
		t.Errorf("prompted %d times for a password already too short; want 1", *asked)
	}
}

func TestUserCLI_NonTerminalNeedsAFlag(t *testing.T) {
	home, as := userDaemon(t)
	withStdin(t, cliPassword+"\n")
	_, _, err := runCLI(t, home, "user", "add", "alice")
	if err == nil || !strings.Contains(err.Error(), "--password-stdin") {
		t.Errorf("user add with piped stdin and no flag: %v", err)
	}

	if _, _, err := runCLI(t, home, "user", "add", "alice", "--no-password"); err != nil {
		t.Fatalf("user add --no-password: %v", err)
	}
	if u, _ := as.GetUser("alice"); u.HasPassword {
		t.Error("--no-password set a password")
	}
}

func TestUserCLI_ListTable(t *testing.T) {
	home, as := userDaemon(t)

	out, _, err := runCLI(t, home, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No users yet") {
		t.Errorf("empty list output: %q", out)
	}

	if err := as.AddUser(auth.User{Username: "alice", Admin: true, Groups: []string{"family", "admins"}}, cliPassword); err != nil {
		t.Fatal(err)
	}
	if err := as.AddUser(auth.User{Username: "bob"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := as.AddUser(auth.User{Username: "carol"}, cliPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := as.Authenticate("alice", cliPassword); err != nil {
		t.Fatal(err)
	}
	if _, err := as.CreateSession("alice", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, home, "user", "disable", "carol"); err != nil {
		t.Fatal(err)
	}

	out, _, err = runCLI(t, home, "user", "list")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("want a header and three rows:\n%s", out)
	}
	if got := strings.Fields(lines[0]); strings.Join(got, " ") != "NAME ADMIN GROUPS STATUS LAST LOGIN SESSIONS" {
		t.Errorf("header = %q", lines[0])
	}
	alice := strings.Fields(lines[1])
	if alice[0] != "alice" || alice[1] != "yes" || alice[2] != "admins,family" || alice[3] != "active" ||
		alice[len(alice)-1] != "1" || strings.Contains(lines[1], "never") {
		t.Errorf("alice row = %q", lines[1])
	}
	for row, want := range map[int][]string{2: {"bob", "-", "-", "no password", "never", "0"}, 3: {"carol", "-", "-", "disabled", "never", "0"}} {
		for _, w := range want {
			if !strings.Contains(lines[row], w) {
				t.Errorf("row %q lacks %q", lines[row], w)
			}
		}
	}
}

func TestUserCLI_GroupsAndOthers(t *testing.T) {
	home, as := userDaemon(t)
	if err := as.AddUser(auth.User{Username: "alice", Admin: true, Groups: []string{"kids"}}, cliPassword); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, home, "user", "groups", "alice", "+family", "-kids")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "alice: family" {
		t.Errorf("groups output = %q", out)
	}
	if _, _, err := runCLI(t, home, "user", "groups", "alice", "family"); err == nil {
		t.Error("a group with no +/- was accepted")
	}

	if _, _, err := runCLI(t, home, "user", "rm", "alice"); err == nil || !strings.Contains(err.Error(), "last enabled admin") {
		t.Errorf("rm last admin: %v", err)
	}
	if out, _, err := runCLI(t, home, "user", "logout", "alice"); err != nil || !strings.Contains(out, "ended 0 session") {
		t.Errorf("logout = %q, %v", out, err)
	}
	if out, _, err := runCLI(t, home, "auth", "rotate-key"); err != nil || !strings.Contains(out, "new signing key") {
		t.Errorf("rotate-key = %q, %v", out, err)
	}
	if _, _, err := runCLI(t, home, "user", "rm"); err == nil || !strings.Contains(err.Error(), "username is required") {
		t.Errorf("rm with no name: %v", err)
	}
}

func TestUserCLI_NoDaemon(t *testing.T) {
	home := newHome(t)
	withStdin(t, cliPassword+"\n")
	for _, args := range [][]string{
		{"user", "list"},
		{"user", "add", "alice", "--password-stdin"},
		{"user", "passwd", "alice", "--password-stdin"},
		{"user", "rm", "alice"},
		{"user", "groups", "alice", "+family"},
		{"user", "logout", "alice"},
		{"auth", "rotate-key"},
	} {
		_, _, err := runCLI(t, home, args...)
		if err == nil || err.Error() != errNoDaemon().Error() {
			t.Errorf("%v with no daemon: %v; want the standard message", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "auth.db")); !os.IsNotExist(err) {
		t.Error("the CLI created auth.db itself")
	}
}

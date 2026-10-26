package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// logBuffer collects everything the server logs.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) logf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	fmt.Fprintf(&l.b, format+"\n", v...)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// authDaemon serves a socket backed by an empty supervisor and, when withAuth
// is set, a real auth store with fast hashing.
func authDaemon(t *testing.T, withAuth bool) (*Client, *auth.Store, *logBuffer) {
	t.Helper()
	home := t.TempDir()
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

	logs := &logBuffer{}
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sockPath, sup, logs.logf)
	if err != nil {
		t.Fatal(err)
	}

	var as *auth.Store
	if withAuth {
		as, err = auth.Open(filepath.Join(home, "auth.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { as.Close() })
		as.Params = auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
		srv.UseAuth(as)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
	})
	return NewClient(sockPath), as, logs
}

const testPassword = "correct horse battery"

func TestAuth_CommandsRoundTrip(t *testing.T) {
	c, as, _ := authDaemon(t, true)

	if _, err := c.UserAdd(UserRequest{Username: "alice", Admin: true, Email: "alice@example.com",
		DisplayName: "Alice", Password: testPassword, AddGroups: []string{"family"}}); err != nil {
		t.Fatalf("add alice: %v", err)
	}
	msg, err := c.UserAdd(UserRequest{Username: "bob"})
	if err != nil {
		t.Fatalf("add bob: %v", err)
	}
	if !strings.Contains(msg, "passwd bob") {
		t.Errorf("adding a user with no password should say how to set one: %q", msg)
	}
	if _, err := c.UserAdd(UserRequest{Username: "bob"}); err == nil {
		t.Error("duplicate add succeeded")
	}

	users, err := c.UserList()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 || users[0].Username != "alice" || users[1].Username != "bob" {
		t.Fatalf("UserList = %+v", users)
	}
	if a := users[0]; !a.Admin || !a.HasPassword || a.Email != "alice@example.com" || a.DisplayName != "Alice" ||
		len(a.Groups) != 1 || a.Groups[0] != "family" {
		t.Errorf("alice = %+v", a)
	}
	if users[1].HasPassword {
		t.Error("bob has a password he was never given")
	}

	if _, err := c.UserPasswd("bob", testPassword); err != nil {
		t.Fatalf("passwd: %v", err)
	}
	if _, err := as.Authenticate("bob", testPassword); err != nil {
		t.Errorf("bob can't authenticate after passwd: %v", err)
	}
	if _, err := c.UserPasswd("bob", "short"); err == nil {
		t.Error("a short password was accepted")
	}

	msg, err = c.UserGroups("bob", []string{"kids", "family"}, []string{"kids"})
	if err != nil {
		t.Fatal(err)
	}
	if msg != "bob: family" {
		t.Errorf("groups message = %q", msg)
	}

	if _, err := as.CreateSession("bob", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if msg, err := c.UserLogout("bob"); err != nil || !strings.Contains(msg, "ended 1 session") {
		t.Errorf("logout = %q, %v", msg, err)
	}

	if _, err := c.UserDisable("bob"); err != nil {
		t.Fatal(err)
	}
	if u, _ := as.GetUser("bob"); !u.Disabled {
		t.Error("disable didn't disable")
	}
	if _, err := c.UserEnable("bob"); err != nil {
		t.Fatal(err)
	}
	if u, _ := as.GetUser("bob"); u.Disabled {
		t.Error("enable didn't enable")
	}

	if _, err := c.UserRemove("alice"); err == nil || !strings.Contains(err.Error(), "last enabled admin") {
		t.Errorf("removing the last admin: %v", err)
	}
	if _, err := c.UserRemove("bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UserRemove("bob"); err == nil {
		t.Error("removing a missing user succeeded")
	}

	before, err := as.CurrentKey()
	if err != nil {
		t.Fatal(err)
	}
	msg, err = c.AuthRotateKey()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := as.CurrentKey()
	if after.ID == before.ID || !strings.Contains(msg, after.ID) {
		t.Errorf("rotate-key = %q; current key %s -> %s", msg, before.ID, after.ID)
	}
}

func TestAuth_UsernameRequired(t *testing.T) {
	c, _, _ := authDaemon(t, true)
	if _, err := c.do(Request{Command: CmdUserRemove}); err == nil || !strings.Contains(err.Error(), "username is required") {
		t.Errorf("user.rm with no user: %v", err)
	}
}

func TestAuth_WithoutStoreSaysSo(t *testing.T) {
	c, _, _ := authDaemon(t, false)
	user := &UserRequest{Username: "alice", Password: testPassword}
	for _, cmd := range []string{CmdUserAdd, CmdUserPasswd, CmdUserRemove, CmdUserList, CmdUserGroups,
		CmdUserDisable, CmdUserEnable, CmdUserLogout, CmdAuthRotate} {
		_, err := c.do(Request{Command: cmd, User: user})
		if err == nil || err.Error() != errNoAuth.Error() {
			t.Errorf("%s without an auth store: %v; want %q", cmd, err, errNoAuth)
		}
	}
	// The rest of the daemon is unaffected.
	if _, err := c.List(); err != nil {
		t.Errorf("list: %v", err)
	}
}

func TestAuth_PasswordNeverLogged(t *testing.T) {
	c, _, logs := authDaemon(t, true)
	const secret = "a-very-distinctive-passphrase"

	if _, err := c.UserAdd(UserRequest{Username: "alice", Password: secret}); err != nil {
		t.Fatal(err)
	}
	_, _ = c.UserPasswd("alice", secret+"!")
	_, _ = c.UserPasswd("ghost", secret)
	_, _ = c.UserAdd(UserRequest{Username: "Bad Name", Password: secret})

	// A request cut off mid-body makes the server log a read or decode
	// failure; the partial body must not appear in it.
	conn, err := net.DialTimeout("unix", c.path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(Request{Command: CmdUserPasswd, User: &UserRequest{Username: "alice", Password: secret}})
	_, _ = conn.Write(body[:len(body)-5])
	_ = conn.(*net.UnixConn).CloseWrite()
	var resp Response
	_ = json.NewDecoder(conn).Decode(&resp)
	conn.Close()
	if strings.Contains(resp.Error, secret) {
		t.Errorf("the error reply echoes the password: %q", resp.Error)
	}

	if strings.Contains(logs.String(), secret) {
		t.Errorf("the password appears in the daemon log:\n%s", logs.String())
	}
}

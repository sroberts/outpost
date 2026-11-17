package ipc

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

// watchDaemon is a running supervisor, with no services, behind a socket.
// The test holds the pieces so it can make changes and shut down on its own
// terms.
func watchDaemon(t *testing.T) (*Client, *supervisor.Supervisor, *Server, string) {
	t.Helper()
	home := t.TempDir()
	sup := newIdleSupervisor(t, home)
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loopDone := make(chan struct{})
	go func() { defer close(loopDone); _ = sup.Run(ctx) }()
	serveDone := make(chan struct{})
	go func() { defer close(serveDone); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-serveDone
		<-loopDone
	})
	return NewClient(sockPath), sup, srv, home
}

func next(t *testing.T, ch <-chan []supervisor.ServiceStatus, within time.Duration) ([]supervisor.ServiceStatus, bool) {
	t.Helper()
	select {
	case v, ok := <-ch:
		return v, ok
	case <-time.After(within):
		t.Fatalf("no frame within %s", within)
		return nil, false
	}
}

func quiet(t *testing.T, ch <-chan []supervisor.ServiceStatus, d time.Duration) {
	t.Helper()
	select {
	case v, ok := <-ch:
		t.Fatalf("unexpected frame (open %v): %+v", ok, v)
	case <-time.After(d):
	}
}

func TestWatch_InitialFrameThenChanges(t *testing.T) {
	c, sup, _, home := watchDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := c.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := next(t, ch, 5*time.Second)
	if !ok {
		t.Fatal("channel closed before the first frame")
	}
	// No services arrives as an empty list, not as nil.
	if first == nil || len(first) != 0 {
		t.Fatalf("first frame = %#v; want an empty list", first)
	}

	// An event signals a change, but the list is the same: no frame.
	if err := sup.RecordEvent(context.Background(), "gateway", "sign-in", "alice"); err != nil {
		t.Fatal(err)
	}
	quiet(t, ch, time.Second)

	// A new service is a frame.
	body := `
name    = "alpha"
runtime = "process"
binary  = "/bin/sleep"
args    = ["3600"]
port    = 19190
storage = "` + filepath.Join(home, "data", "alpha") + `"
`
	if err := os.WriteFile(filepath.Join(home, "services", "alpha.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Apply(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := next(t, ch, time.Until(deadline))
		if !ok {
			t.Fatal("channel closed")
		}
		if len(got) == 1 && got[0].Name == "alpha" && got[0].Status == "running" {
			break
		}
	}

	// Stopping it is another.
	if _, err := c.Stop("alpha"); err != nil {
		t.Fatal(err)
	}
	for {
		got, ok := next(t, ch, time.Until(deadline.Add(5*time.Second)))
		if !ok {
			t.Fatal("channel closed")
		}
		if len(got) == 1 && got[0].Desired == "stopped" && got[0].Status != "running" {
			break
		}
	}
}

func TestWatch_CancelClosesTheChannel(t *testing.T) {
	c, _, _, _ := watchDaemon(t)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := c.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next(t, ch, 5*time.Second)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a frame after cancel; want the channel closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel still open after cancel")
	}
}

// Closing the server ends a stream whose client is still there, so shutdown
// isn't held up waiting for it.
func TestWatch_ServerCloseEndsTheStream(t *testing.T) {
	c, _, srv, _ := watchDaemon(t)
	ch, err := c.Watch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	next(t, ch, 5*time.Second)
	srv.Close()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("a frame after close; want the channel closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open after the server closed")
	}
}

// A daemon from before watch answers with an error, which Watch returns
// rather than a channel that never delivers.
func TestWatch_OldDaemon(t *testing.T) {
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 4096)
		_, _ = conn.Read(buf) // what the client asked doesn't matter to this daemon
		_ = json.NewEncoder(conn).Encode(Response{OK: false, Error: `unknown command "watch"`})
	}()

	ch, err := NewClient(sockPath).Watch(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unknown command "watch"`) {
		t.Fatalf("Watch = %v, %v; want the daemon's error", ch, err)
	}
}

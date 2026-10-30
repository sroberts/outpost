package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// loginTailnet is a router whose every node is waiting for a login.
type loginTailnet struct{}

func (loginTailnet) Attach(context.Context, *manifest.Service) error { return nil }
func (loginTailnet) Detach(context.Context, string) error            { return nil }
func (loginTailnet) TailnetStatus(string) (supervisor.TailnetStatus, bool) {
	return supervisor.TailnetStatus{State: supervisor.TailnetNeedsLogin, LoginURL: "https://login.tailscale.com/a/abc"}, true
}

// tailnetDaemon serves a home with one service, alpha, whose node needs a
// login.
func tailnetDaemon(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "op")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	for _, d := range []string{"services", "logs", "backups"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeManifest(t, home, "alpha", freePort(t))

	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		BackupDir:      filepath.Join(home, "backups"),
		Tailnet:        loginTailnet{},
		Interval:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ipc.Listen(filepath.Join(home, "outpost.sock"), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	loopDone, serveDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(loopDone); _ = sup.Run(ctx) }()
	go func() { defer close(serveDone); _ = srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-serveDone
		<-loopDone
		store.Close()
	})
	return home
}

// A node waiting for a login reads as one in list, and status says where.
func TestTailnet_ListAndStatusShowANodeNeedingLogin(t *testing.T) {
	home := tailnetDaemon(t)
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "needs login") {
		t.Errorf("list:\n%s", out)
	}

	out, _, err = runCLI(t, home, "status", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alpha (needs login)", "login:         https://login.tailscale.com/a/abc", "outpost config --authkey"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// configDaemon serves an empty home whose daemon started with the
// config.toml written here, as `outpost serve` records it.
func configDaemon(t *testing.T, body string) string {
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
	writeConfig(t, home, body)
	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    filepath.Join(home, "services"),
		Store:          store,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
		BackupDir:      filepath.Join(home, "backups"),
		Interval:       50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ipc.Listen(filepath.Join(home, "outpost.sock"), sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseConfig(cfg.Fingerprint())
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

// Changing config.toml under a running daemon is called out wherever the
// daemon's state is shown, since it applies only after a restart.
func TestConfigChanged_SaysToRestart(t *testing.T) {
	home := configDaemon(t, "[tailscale]\nenabled = false\n")

	_, stderr, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "restart") {
		t.Fatalf("warned with the file unchanged: %q", stderr)
	}

	// Rewording the file without changing a setting is not a change.
	writeConfig(t, home, "# mine\n[tailscale]\n  enabled = false # for now\n")
	if _, stderr, _ = runCLI(t, home, "list"); strings.Contains(stderr, "restart") {
		t.Fatalf("warned about a comment: %q", stderr)
	}

	writeConfig(t, home, "[tailscale]\nenabled = true\n")
	_, stderr, err = runCLI(t, home, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, restartToApply) {
		t.Errorf("list stderr = %q", stderr)
	}
	stdout, _, err := runCLI(t, home, "config")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, restartToApply) {
		t.Errorf("config = %q", stdout)
	}
}

// --tailscale compares with what the daemon is running, not with the file's
// previous value, so setting it back says nothing needs restarting.
func TestSetTailscale_ComparesWithTheDaemon(t *testing.T) {
	home := configDaemon(t, "[tailscale]\nenabled = false\n")

	stdout, _, err := runCLI(t, home, "config", "--tailscale", "on")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "restart it to apply") {
		t.Errorf("turning it on = %q", stdout)
	}
	stdout, _, err = runCLI(t, home, "config", "--tailscale", "off")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "restart") {
		t.Errorf("turning it back off = %q", stdout)
	}
}

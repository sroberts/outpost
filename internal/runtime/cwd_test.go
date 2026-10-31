package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// processCwd asks a running fakeservice for its working directory.
func processCwd(t *testing.T, port int) string {
	t.Helper()
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/cwd", port))
	if err != nil {
		t.Fatalf("read cwd: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// sameDir compares directories through symlinks: on macOS a temp dir is
// reached through /var, which getcwd reports as /private/var.
func sameDir(t *testing.T, a, b string) bool {
	t.Helper()
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		t.Fatal(err)
	}
	return ra == rb
}

// A service runs in its storage directory, not the daemon's, which is the
// Outpost home: an app writing ./config.toml must not land on Outpost's own.
func TestProcessRuntime_RunsInStorageDir(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), h) })
	waitHealthy(t, r, h, 5*time.Second)

	if got := processCwd(t, svc.Port); !sameDir(t, got, svc.ExpandStorage()) {
		t.Errorf("cwd = %q; want the storage dir %q", got, svc.ExpandStorage())
	}
}

// A relative binary path still means what it did before services got their
// own working directory: relative to the daemon's.
func TestProcessRuntime_RelativeBinaryResolvesAgainstDaemonDir(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(wd, svc.Binary)
	if err != nil {
		t.Fatal(err)
	}
	svc.Binary = rel

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background(), h) })
	waitHealthy(t, r, h, 5*time.Second)
}

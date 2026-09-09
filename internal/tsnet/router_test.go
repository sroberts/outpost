package tsnet

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// These cover everything that can be checked without a tailnet: configuration,
// state-directory layout and lifecycle bookkeeping. Attaching a real node needs
// credentials, so that stays manual for v1 (§13).

func TestNew_RequiresStateDir(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New without a StateDir: want error, got nil")
	}
}

func TestNew_CreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tsnet")
	if _, err := New(Options{StateDir: dir}); err != nil {
		t.Fatalf("New: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("state dir not created: %v", err)
	}
}

// Node state must be per-service and stable, so identities survive a restart
// rather than re-registering (§7).
func TestStateDirFor_IsPerService(t *testing.T) {
	root := t.TempDir()
	r, err := New(Options{StateDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := r.StateDirFor("plumbline"), filepath.Join(root, "plumbline"); got != want {
		t.Errorf("StateDirFor = %q; want %q", got, want)
	}
	if r.StateDirFor("a") == r.StateDirFor("b") {
		t.Error("two services share a state directory")
	}
}

func TestAttach_RejectsFunnel(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc := &manifest.Service{Name: "a", TailscaleName: "a", Port: 1234, Funnel: true}
	if err := r.Attach(context.Background(), svc); err == nil {
		t.Fatal("Attach with funnel = true: want error, got nil")
	}
}

func TestAttach_RequiresTailscaleName(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc := &manifest.Service{Name: "a", Port: 1234}
	if err := r.Attach(context.Background(), svc); err == nil {
		t.Fatal("Attach without a tailscale_name: want error, got nil")
	}
}

// Detaching something that was never attached is how the reconcile loop cleans
// up, so it must be a no-op rather than an error.
func TestDetach_UnknownServiceIsANoOp(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Detach(context.Background(), "never-attached"); err != nil {
		t.Errorf("Detach of an unattached service: %v", err)
	}
	if got := r.Attached(); len(got) != 0 {
		t.Errorf("Attached = %v; want empty", got)
	}
}

func TestClose_WithNoNodes(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close with no nodes: %v", err)
	}
}

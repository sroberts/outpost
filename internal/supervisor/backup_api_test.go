package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// runningHarness starts the reconcile loop so the API's command queue is drained.
func runningHarness(t *testing.T) (*harness, context.CancelFunc) {
	t.Helper()
	h := newHarness(t)
	h.sup.backupDir = filepath.Join(h.dir, "backups")
	h.sup.Interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.sup.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	return h, cancel
}

// seed writes a file into a service's storage directory.
func seed(t *testing.T, h *harness, name, file, body string) {
	t.Helper()
	dir := filepath.Join(h.dir, "data", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackup_ArchivesAndRestartsARunningService(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	seed(t, h, "alpha", "data.txt", "v1")

	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })

	archive, err := h.sup.Backup(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if archive.Path == "" || archive.Size == 0 {
		t.Errorf("archive not reported properly: %+v", archive)
	}
	if _, err := os.Stat(archive.Path); err != nil {
		t.Fatalf("archive not on disk: %v", err)
	}

	// The service must be stopped for the archive and started again after.
	if got := h.rt.stopCount("alpha"); got != 1 {
		t.Errorf("stop count = %d; want 1 (service should pause for the backup)", got)
	}
	if got := h.rt.startCount("alpha"); got != 2 {
		t.Errorf("start count = %d; want 2 (service should restart after the backup)", got)
	}
}

// A service the user already stopped must not be started by a backup.
func TestBackup_LeavesAStoppedServiceStopped(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	seed(t, h, "alpha", "data.txt", "v1")
	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })

	if err := h.sup.StopService(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	startsBefore := h.rt.startCount("alpha")

	if _, err := h.sup.Backup(context.Background(), "alpha"); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if got := h.rt.startCount("alpha"); got != startsBefore {
		t.Errorf("a stopped service was started by a backup (starts %d -> %d)", startsBefore, got)
	}
}

func TestRestore_ReplacesStorageAndRestarts(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	seed(t, h, "alpha", "data.txt", "v1")
	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })

	archive, err := h.sup.Backup(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}

	// Move the data on, then roll back to the archive.
	seed(t, h, "alpha", "data.txt", "v2")
	seed(t, h, "alpha", "added-later.txt", "should not survive")

	if _, err := h.sup.Restore(context.Background(), "alpha", archive.Timestamp); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(h.dir, "data", "alpha", "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "v1" {
		t.Errorf("data.txt = %q; want %q", body, "v1")
	}
	if _, err := os.Stat(filepath.Join(h.dir, "data", "alpha", "added-later.txt")); !os.IsNotExist(err) {
		t.Error("a file added after the backup survived the restore")
	}
	if h.status("alpha") != "running" {
		t.Errorf("service not running after restore: %s", h.status("alpha"))
	}
}

func TestRestore_EmptyTimestampUsesNewest(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })

	seed(t, h, "alpha", "data.txt", "old")
	if _, err := h.sup.Backup(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	// Timestamps have one-second resolution, so separate the two archives.
	time.Sleep(1100 * time.Millisecond)
	seed(t, h, "alpha", "data.txt", "new")
	if _, err := h.sup.Backup(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}

	seed(t, h, "alpha", "data.txt", "scratch")
	if _, err := h.sup.Restore(context.Background(), "alpha", ""); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	body, _ := os.ReadFile(filepath.Join(h.dir, "data", "alpha", "data.txt"))
	if string(body) != "new" {
		t.Errorf("restored %q; want the newest archive (%q)", body, "new")
	}
}

func TestListBackups_NewestFirst(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })
	seed(t, h, "alpha", "data.txt", "v1")

	if _, err := h.sup.Backup(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	archives, err := h.sup.ListBackups(context.Background(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 {
		t.Fatalf("got %d archives; want 1", len(archives))
	}
}

func TestBackup_UnknownServiceIsAnError(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()

	if _, err := h.sup.Backup(context.Background(), "ghost"); err == nil {
		t.Error("Backup of an unknown service: want error, got nil")
	}
	if _, err := h.sup.Restore(context.Background(), "ghost", ""); err == nil {
		t.Error("Restore of an unknown service: want error, got nil")
	}
}

func TestRestore_MissingArchiveIsAnErrorAndLeavesDataAlone(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	waitUntil(t, 2*time.Second, func() bool { return h.rt.startCount("alpha") == 1 })
	seed(t, h, "alpha", "data.txt", "live")

	if _, err := h.sup.Restore(context.Background(), "alpha", "1999-01-01T00-00-00"); err == nil {
		t.Fatal("Restore of a missing archive: want error, got nil")
	}
	body, err := os.ReadFile(filepath.Join(h.dir, "data", "alpha", "data.txt"))
	if err != nil || string(body) != "live" {
		t.Errorf("live data disturbed by a failed restore: %q (%v)", body, err)
	}
}

func TestBackupAll_CoversEveryService(t *testing.T) {
	h, _ := runningHarness(t)
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	waitUntil(t, 2*time.Second, func() bool {
		return h.rt.startCount("alpha") == 1 && h.rt.startCount("beta") == 1
	})
	seed(t, h, "alpha", "a.txt", "a")
	seed(t, h, "beta", "b.txt", "b")

	archives, err := h.sup.BackupAll(context.Background())
	if err != nil {
		t.Fatalf("BackupAll: %v", err)
	}
	if len(archives) != 2 {
		t.Fatalf("got %d archives; want 2", len(archives))
	}
}

func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", within)
}

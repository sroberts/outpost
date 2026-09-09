package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seedStorage builds a small directory tree to archive.
func seedStorage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "db.sqlite"), "primary data")
	mustWrite(t, filepath.Join(dir, "nested", "deep", "file.txt"), "nested data")
	if err := os.MkdirAll(filepath.Join(dir, "emptydir"), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndExtract_RoundTrip(t *testing.T) {
	storage := seedStorage(t)
	archive := filepath.Join(t.TempDir(), "backup.tar.gz")

	if err := Create(storage, archive); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fi, err := os.Stat(archive); err != nil || fi.Size() == 0 {
		t.Fatalf("archive missing or empty: %v", err)
	}

	restored := filepath.Join(t.TempDir(), "restored")
	if err := Extract(archive, restored); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	for path, want := range map[string]string{
		"db.sqlite":            "primary data",
		"nested/deep/file.txt": "nested data",
	} {
		got, err := os.ReadFile(filepath.Join(restored, path))
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q; want %q", path, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(restored, "emptydir")); err != nil || !fi.IsDir() {
		t.Errorf("empty directory not preserved: %v", err)
	}
}

func TestCreate_PreservesFileModes(t *testing.T) {
	storage := t.TempDir()
	mustWrite(t, filepath.Join(storage, "secret.key"), "sensitive")
	if err := os.Chmod(filepath.Join(storage, "secret.key"), 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := Create(storage, archive); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "r")
	if err := Extract(archive, restored); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(filepath.Join(restored, "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored mode = %#o; want 0600", perm)
	}
}

// Restore must replace the storage directory, not merge into it: stale files
// left behind would be indistinguishable from restored ones.
func TestExtract_ReplacesExistingContents(t *testing.T) {
	storage := seedStorage(t)
	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := Create(storage, archive); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(storage, "written-after-backup.txt"), "stale")
	if err := Extract(archive, storage); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	if _, err := os.Stat(filepath.Join(storage, "written-after-backup.txt")); !os.IsNotExist(err) {
		t.Error("a file created after the backup survived the restore")
	}
	if _, err := os.Stat(filepath.Join(storage, "db.sqlite")); err != nil {
		t.Errorf("restored file missing: %v", err)
	}
}

// A crafted archive must not be able to write outside the target directory.
func TestExtract_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("pwned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../escaped.txt", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	if err := os.WriteFile(archive, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "target")
	err := Extract(archive, target)
	if err == nil {
		t.Fatal("Extract of a traversing archive: want error, got nil")
	}
	if !strings.Contains(err.Error(), "escapes") {
		t.Errorf("error should name the traversal: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "escaped.txt")); statErr == nil {
		t.Fatal("traversing entry was written outside the target directory")
	}
}

func TestExtract_RejectsAbsolutePaths(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "abs.tar.gz")

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	body := []byte("x")
	tw.WriteHeader(&tar.Header{Name: "/etc/outpost-pwned", Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write(body)
	tw.Close()
	gz.Close()
	os.WriteFile(archive, buf.Bytes(), 0o600)

	if err := Extract(archive, filepath.Join(dir, "target")); err == nil {
		t.Fatal("Extract of an absolute-path archive: want error, got nil")
	}
}

func TestCreate_MissingStorageIsAnError(t *testing.T) {
	if err := Create(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "b.tar.gz")); err == nil {
		t.Fatal("Create over a missing storage dir: want error, got nil")
	}
}

// A failed Create must not leave a truncated archive that looks restorable.
func TestCreate_NoPartialArchiveOnFailure(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "b.tar.gz")
	if err := Create(filepath.Join(dir, "does-not-exist"), archive); err == nil {
		t.Fatal("want error")
	}
	if _, err := os.Stat(archive); err == nil {
		t.Error("a partial archive was left behind after a failed Create")
	}
}

func TestArchivePathAndList(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 9, 8, 23, 4, 11, 0, time.UTC)

	path := ArchivePath(root, "plumbline", at)
	want := filepath.Join(root, "plumbline", "2026-09-08T23-04-11.tar.gz")
	if path != want {
		t.Errorf("ArchivePath = %q; want %q", path, want)
	}

	storage := seedStorage(t)
	for _, ts := range []time.Time{at, at.Add(time.Hour), at.Add(2 * time.Hour)} {
		if err := Create(storage, ArchivePath(root, "plumbline", ts)); err != nil {
			t.Fatal(err)
		}
	}

	archives, err := List(root, "plumbline")
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 3 {
		t.Fatalf("List returned %d archives; want 3", len(archives))
	}
	// Newest first, so `outpost restore` without a timestamp has an obvious default.
	if !archives[0].CreatedAt.After(archives[1].CreatedAt) {
		t.Errorf("archives not ordered newest first: %v then %v", archives[0].Timestamp, archives[1].Timestamp)
	}
	if archives[0].Size == 0 {
		t.Error("archive size not reported")
	}
}

func TestList_UnknownServiceIsEmptyNotAnError(t *testing.T) {
	archives, err := List(t.TempDir(), "never-backed-up")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(archives) != 0 {
		t.Errorf("got %d archives; want 0", len(archives))
	}
}

func TestFind_ByTimestamp(t *testing.T) {
	root := t.TempDir()
	storage := seedStorage(t)
	at := time.Date(2026, 9, 8, 23, 4, 11, 0, time.UTC)
	if err := Create(storage, ArchivePath(root, "svc", at)); err != nil {
		t.Fatal(err)
	}

	found, err := Find(root, "svc", "2026-09-08T23-04-11")
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	if found.Timestamp != "2026-09-08T23-04-11" {
		t.Errorf("Timestamp = %q", found.Timestamp)
	}

	if _, err := Find(root, "svc", "1999-01-01T00-00-00"); err == nil {
		t.Error("Find for a missing timestamp: want error, got nil")
	}
}

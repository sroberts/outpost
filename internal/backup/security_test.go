package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeArchive builds a tar.gz from raw headers, for archives that legitimate
// tooling would not produce.
func writeArchive(t *testing.T, path string, entries func(*tar.Writer)) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A symlink with an absolute target, followed by a file written through it,
// must not place data outside the restore target.
func TestExtract_RejectsAbsoluteSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "evil.tar.gz")
	body := []byte("pwned")

	writeArchive(t, archive, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "link/owned.txt", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})

	err := Extract(archive, filepath.Join(dir, "target"))
	if err == nil {
		t.Fatal("Extract of an absolute-symlink archive: want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "owned.txt")); statErr == nil {
		t.Fatalf("data was written outside the restore target: %v", err)
	}
}

// The same escape via a relative symlink.
func TestExtract_RejectsRelativeSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "evil.tar.gz")
	body := []byte("pwned")

	writeArchive(t, archive, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "../outside", Mode: 0o777}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "link/owned.txt", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})

	err := Extract(archive, filepath.Join(dir, "target"))
	if err == nil {
		t.Fatal("Extract of a relative symlink escape: want error, got nil")
	}
	if _, statErr := os.Stat(filepath.Join(outside, "owned.txt")); statErr == nil {
		t.Fatal("data was written outside the restore target")
	}
}

// A directory entry may not be replaced by a symlink that later entries
// traverse, even when every individual name looks contained.
func TestExtract_RejectsWritingThroughASymlinkedParent(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	archive := filepath.Join(dir, "evil.tar.gz")
	body := []byte("pwned")

	writeArchive(t, archive, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "data", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "data/nested/owned.txt", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})

	_ = Extract(archive, filepath.Join(dir, "target"))
	if _, err := os.Stat(filepath.Join(outside, "nested", "owned.txt")); err == nil {
		t.Fatal("data was written outside the restore target through a symlinked parent")
	}
}

// Symlinks that stay inside the tree are legitimate and must survive a round trip.
func TestExtract_KeepsContainedSymlinks(t *testing.T) {
	storage := t.TempDir()
	if err := os.WriteFile(filepath.Join(storage, "real.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.txt", filepath.Join(storage, "alias.txt")); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "b.tar.gz")
	if err := Create(storage, archive); err != nil {
		t.Fatalf("Create: %v", err)
	}
	restored := filepath.Join(t.TempDir(), "r")
	if err := Extract(archive, restored); err != nil {
		t.Fatalf("Extract of a contained symlink: %v", err)
	}

	fi, err := os.Lstat(filepath.Join(restored, "alias.txt"))
	if err != nil {
		t.Fatalf("symlink not restored: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("alias.txt was restored as a regular file, not a symlink")
	}
	body, err := os.ReadFile(filepath.Join(restored, "alias.txt"))
	if err != nil || string(body) != "data" {
		t.Errorf("symlink does not resolve: %q (%v)", body, err)
	}
}

// A failed extraction must leave the existing storage directory intact.
func TestExtract_FailureLeavesExistingDataIntact(t *testing.T) {
	dir := t.TempDir()
	storage := filepath.Join(dir, "storage")
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(storage, "live.txt"), []byte("live data"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	archive := filepath.Join(dir, "evil.tar.gz")
	writeArchive(t, archive, func(tw *tar.Writer) {
		if err := tw.WriteHeader(&tar.Header{Name: "../escape.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o600}); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})

	if err := Extract(archive, storage); err == nil {
		t.Fatal("want error")
	}
	body, err := os.ReadFile(filepath.Join(storage, "live.txt"))
	if err != nil || string(body) != "live data" {
		t.Errorf("live data lost after a failed restore: %q (%v)", body, err)
	}
}

func TestSafeJoin_RejectsEscapes(t *testing.T) {
	root := "/srv/data"
	for _, name := range []string{"../x", "a/../../x", "/etc/passwd", "a/b/../../../x"} {
		if _, err := safeJoin(root, name); err == nil {
			t.Errorf("safeJoin(%q) = nil error; want a rejection", name)
		} else if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("safeJoin(%q) error should mention the escape: %v", name, err)
		}
	}
	for _, name := range []string{"a", "a/b/c", "./a"} {
		if _, err := safeJoin(root, name); err != nil {
			t.Errorf("safeJoin(%q) rejected a contained path: %v", name, err)
		}
	}
}

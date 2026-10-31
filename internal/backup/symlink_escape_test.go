package backup

import (
	"archive/tar"
	"os"
	"path/filepath"
	"testing"
)

// A link target that cleans to somewhere inside the tree but climbs out of an
// earlier symlink on disk must be refused, or the restored storage directory
// carries a link that a service reading through it follows outside.
func TestExtract_RejectsSymlinkClimbingOutOfAnotherSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "evil.tar.gz")
	writeArchive(t, archive, func(tw *tar.Writer) {
		for _, h := range []*tar.Header{
			{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o700},
			{Name: "sub/link", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o777},
			{Name: "x", Typeflag: tar.TypeSymlink, Linkname: "sub/link/..", Mode: 0o777},
		} {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatalf("WriteHeader: %v", err)
			}
		}
	})

	storage := filepath.Join(dir, "storage")
	if err := Extract(archive, storage); err == nil {
		t.Fatal("Extract of a link climbing out of another link: want error, got nil")
	}
	if _, err := os.Stat(filepath.Join(storage, "x", "secret.txt")); err == nil {
		t.Fatal("restored tree reaches outside the storage directory")
	}
}

// A regular file entry at the path of a planted symlink must not be written
// through it.
func TestExtract_RejectsFileWrittenOverASymlink(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "evil.tar.gz")
	body := []byte("pwned")
	writeArchive(t, archive, func(tw *tar.Writer) {
		for _, h := range []*tar.Header{
			{Name: "sub/", Typeflag: tar.TypeDir, Mode: 0o700},
			{Name: "sub/link", Typeflag: tar.TypeSymlink, Linkname: "..", Mode: 0o777},
			{Name: "y", Typeflag: tar.TypeSymlink, Linkname: "sub/link/../../PWNED", Mode: 0o777},
			{Name: "y", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600},
		} {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatalf("WriteHeader: %v", err)
			}
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})

	storage := filepath.Join(base, "storage")
	if err := Extract(archive, storage); err == nil {
		t.Fatal("Extract writing through a planted symlink: want error, got nil")
	}
	for _, p := range []string{filepath.Join(dir, "a", "PWNED"), filepath.Join(base, "PWNED"), filepath.Join(dir, "PWNED")} {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("data was written outside the restore target at %s", p)
		}
	}
}

// Even a contained symlink may not have a later entry written through it.
func TestExtract_RejectsFileOverAContainedSymlink(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "evil.tar.gz")
	body := []byte("x")
	writeArchive(t, archive, func(tw *tar.Writer) {
		for _, h := range []*tar.Header{
			{Name: "real.txt", Typeflag: tar.TypeReg, Size: 0, Mode: 0o600},
			{Name: "alias", Typeflag: tar.TypeSymlink, Linkname: "real.txt", Mode: 0o777},
			{Name: "alias", Typeflag: tar.TypeReg, Size: int64(len(body)), Mode: 0o600},
		} {
			if err := tw.WriteHeader(h); err != nil {
				t.Fatalf("WriteHeader: %v", err)
			}
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatalf("Write: %v", err)
		}
	})
	if err := Extract(archive, filepath.Join(dir, "storage")); err == nil {
		t.Fatal("want error writing a file over a symlink entry")
	}
}

func TestCheckLinkTarget(t *testing.T) {
	for _, tc := range []struct {
		name, link string
		ok         bool
	}{
		{"alias.txt", "real.txt", true},
		{"a/b/l", "../x", true},
		{"a/b/l", "../../x/y", true},
		{"a/l", "./x/./y", true},
		{"l", "../x", false},
		{"x", "sub/link/..", false},
		{"y", "sub/link/../../PWNED", false},
		{"a/b/l", "../c/../d", false},
	} {
		err := checkLinkTarget(tc.name, tc.link)
		if (err == nil) != tc.ok {
			t.Errorf("checkLinkTarget(%q, %q) = %v, want ok=%v", tc.name, tc.link, err, tc.ok)
		}
	}
}

package acquire

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name string
	body string
	mode os.FileMode
}

func tarGz(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: e.name, Mode: int64(mode), Size: int64(len(e.body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipBytes(t *testing.T, entries []entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeTemp(t *testing.T, body []byte, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The shape yarr ships: one binary in a zip, named after the service.
func TestExtractBinary_ZipWithOneBinary(t *testing.T) {
	src := writeTemp(t, zipBytes(t, []entry{{name: "yarr", body: "#!/bin/sh\n", mode: 0o755}}), "a.zip")
	dest := filepath.Join(t.TempDir(), "yarr")

	member, err := extractBinary(src, dest, "yarr", archiveZip)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if member != "yarr" {
		t.Errorf("member = %q", member)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "#!/bin/sh\n" {
		t.Errorf("content = %q", got)
	}
}

// The shape goreleaser ships: the binary beside a licence and a readme, often
// under a directory named for the release.
func TestExtractBinary_TarGzWithTheUsualClutter(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "svc_1.2.3_linux_amd64/LICENSE", body: "MIT"},
		{name: "svc_1.2.3_linux_amd64/README.md", body: "# svc"},
		{name: "svc_1.2.3_linux_amd64/completions/svc.bash", body: "complete"},
		{name: "svc_1.2.3_linux_amd64/svc", body: "ELF", mode: 0o755},
	}), "a.tar.gz")
	dest := filepath.Join(t.TempDir(), "svc")

	member, err := extractBinary(src, dest, "svc", archiveTarGz)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if member != "svc_1.2.3_linux_amd64/svc" {
		t.Errorf("member = %q; want the nested binary", member)
	}
	if got, _ := os.ReadFile(dest); string(got) != "ELF" {
		t.Errorf("content = %q", got)
	}
}

// A name match is not required when only one file could be the binary.
func TestExtractBinary_SoleExecutableWinsWithoutANameMatch(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "LICENSE", body: "MIT"},
		{name: "some-other-name", body: "ELF", mode: 0o755},
	}), "a.tar.gz")
	dest := filepath.Join(t.TempDir(), "svc")

	if _, err := extractBinary(src, dest, "svc", archiveTarGz); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "ELF" {
		t.Errorf("content = %q", got)
	}
}

// Guessing wrong installs the wrong file, and the failure surfaces much later
// as an exec error. Better to say what was found.
func TestExtractBinary_AmbiguityIsAnErrorNamingTheCandidates(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "alpha", body: "a", mode: 0o755},
		{name: "beta", body: "b", mode: 0o755},
	}), "a.tar.gz")

	_, err := extractBinary(src, filepath.Join(t.TempDir(), "svc"), "svc", archiveTarGz)
	if err == nil {
		t.Fatal("two executables: want an error")
	}
	for _, want := range []string{"alpha", "beta", "--name"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Zip slip: an entry named to climb out of the destination. The archive's own
// paths are never used to place anything — they only choose a member — so this
// cannot escape. The test pins that property.
func TestExtractBinary_TraversalEntryCannotEscape(t *testing.T) {
	for _, evil := range []string{"../../../../tmp/outpost-pwned", "/tmp/outpost-pwned", "..\\..\\outpost-pwned"} {
		for _, tc := range []struct {
			kind int
			body []byte
			ext  string
		}{
			{archiveTarGz, tarGz(t, []entry{{name: evil, body: "owned", mode: 0o755}}), "a.tar.gz"},
			{archiveZip, zipBytes(t, []entry{{name: evil, body: "owned", mode: 0o755}}), "a.zip"},
		} {
			src := writeTemp(t, tc.body, tc.ext)
			dir := t.TempDir()
			dest := filepath.Join(dir, "svc")

			// It may be chosen as the sole candidate — that is fine. What must
			// hold is that it lands at dest and nowhere else.
			if _, err := extractBinary(src, dest, "svc", tc.kind); err != nil {
				continue
			}
			if _, err := os.Stat("/tmp/outpost-pwned"); err == nil {
				os.Remove("/tmp/outpost-pwned")
				t.Fatalf("entry %q escaped to /tmp", evil)
			}
			if got, _ := os.ReadFile(dest); string(got) != "owned" {
				t.Errorf("entry %q: content landed elsewhere (%q)", evil, got)
			}
		}
	}
}

// An archive holding nothing installable must say so rather than install a
// readme.
func TestExtractBinary_NoBinaryIsAClearError(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "README.md", body: "# svc"},
		{name: "LICENSE", body: "MIT"},
	}), "a.tar.gz")

	_, err := extractBinary(src, filepath.Join(t.TempDir(), "svc"), "svc", archiveTarGz)
	if err == nil {
		t.Fatal("an archive with no binary was accepted")
	}
	if !strings.Contains(err.Error(), "no file that looks like a binary") {
		t.Errorf("error = %v", err)
	}
}

func TestArchiveKind(t *testing.T) {
	for name, want := range map[string]int{
		"svc.tar.gz": archiveTarGz, "svc.tgz": archiveTarGz, "SVC.TAR.GZ": archiveTarGz,
		"svc.zip": archiveZip, "svc.ZIP": archiveZip,
		"svc": archiveNone, "svc.tar.xz": archiveNone, "svc.deb": archiveNone,
	} {
		if got := archiveKind(name); got != want {
			t.Errorf("archiveKind(%q) = %d; want %d", name, got, want)
		}
	}
}

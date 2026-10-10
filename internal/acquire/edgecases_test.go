package acquire

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A per-asset sidecar with an extension the deny-lists do not know must not
// count as a bare binary and hide the archive it describes.
func TestSelectAsset_SidecarsDoNotHideTheArchive(t *testing.T) {
	for _, sidecar := range []string{
		"svc_linux_amd64.tar.gz.sha512",
		"svc_linux_amd64.tar.gz.md5",
		"svc_linux_amd64.tar.gz.sigstore",
		"svc_linux_amd64.tar.gz.intoto.jsonl",
		"svc_linux_amd64.tar.gz.whatever",
	} {
		h := newFakeHub(t)
		h.release = &Release{Tag: "v1"}
		h.addAsset("svc_linux_amd64.tar.gz", []byte("tarball"))
		h.addAsset(sidecar, []byte("not a binary"))

		got, err := h.client().SelectAsset(h.release, &Descriptor{})
		if err != nil {
			t.Errorf("%s: SelectAsset: %v", sidecar, err)
			continue
		}
		if got.Name != "svc_linux_amd64.tar.gz" {
			t.Errorf("%s: selected %q; want the archive", sidecar, got.Name)
		}
	}
}

// A bare .gz is a compressed binary, not a binary: installing its bytes fails
// at exec time.
func TestSelectAsset_CompressedBinaryIsNotABareBinary(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-x86_64-unknown-linux-gnu.gz", []byte("gzip"))
	h.addAsset("svc-x86_64-unknown-linux-gnu.tar.gz", []byte("tarball"))

	c := h.client()
	c.GOOS, c.GOARCH = "linux", "amd64"
	got, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "svc-x86_64-unknown-linux-gnu.tar.gz" {
		t.Errorf("selected %q; want the tarball", got.Name)
	}
}

// "darwin" contains "win" and "macchina" contains "mac". Matching platform
// words as raw substrings handed a host another platform's binary.
func TestSelectAsset_PlatformWordsMatchWholeWords(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-darwin-amd64.tar.gz", []byte("mach-o"))
	h.addAsset("svc-windows-amd64.zip", []byte("pe"))

	c := h.client()
	c.GOOS, c.GOARCH = "windows", "amd64"
	got, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "svc-windows-amd64.zip" {
		t.Errorf("GOOS=windows selected %q", got.Name)
	}

	h2 := newFakeHub(t)
	h2.release = &Release{Tag: "v1"}
	h2.addAsset("macchina-linux-x86_64", []byte("elf"))
	c2 := h2.client()
	c2.GOOS, c2.GOARCH = "darwin", "amd64"
	if got, err := c2.SelectAsset(h2.release, &Descriptor{}); err == nil {
		t.Errorf("GOOS=darwin selected %q; a project named mac-something is not macOS", got.Name)
	}
}

// Word matching must still accept the spellings that carry a version or a
// bitness, and the aliases that only exist because of it.
func TestSelectAsset_WordMatchingKeepsTheCommonSpellings(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, asset string }{
		{"windows", "amd64", "svc-win64-x64.zip"},
		{"linux", "arm", "svc_linux_armv7l.tar.gz"},
		{"linux", "386", "svc-linux-i386"},
		{"linux", "386", "svc-linux-i686"},
		{"darwin", "arm64", "svc-aarch64-apple-darwin.tar.gz"},
		{"linux", "amd64", "svc_Linux_x86_64.tar.gz"},
	} {
		h := newFakeHub(t)
		h.release = &Release{Tag: "v1"}
		h.addAsset(tc.asset, []byte("x"))

		c := h.client()
		c.GOOS, c.GOARCH = tc.goos, tc.goarch
		if got, err := c.SelectAsset(h.release, &Descriptor{}); err != nil || got.Name != tc.asset {
			t.Errorf("%s/%s: got %q, %v; want %q", tc.goos, tc.goarch, got.Name, err, tc.asset)
		}
	}
}

// The same build in two archive formats is one build, not a variant; the
// format native to the platform is taken and the log says what happened.
func TestSelectAsset_SameBuildInTwoFormats(t *testing.T) {
	for goos, want := range map[string]string{"linux": "svc_%s_amd64.tar.gz", "windows": "svc_%s_amd64.zip"} {
		h := newFakeHub(t)
		h.release = &Release{Tag: "v1"}
		h.addAsset(fmt.Sprintf("svc_%s_amd64.zip", goos), []byte("zip"))
		h.addAsset(fmt.Sprintf("svc_%s_amd64.tar.gz", goos), []byte("tgz"))

		var logged string
		c := h.client()
		c.GOOS, c.GOARCH = goos, "amd64"
		c.Logf = func(f string, a ...any) { logged = fmt.Sprintf(f, a...) }

		got, err := c.SelectAsset(h.release, &Descriptor{})
		if err != nil {
			t.Errorf("%s: SelectAsset: %v", goos, err)
			continue
		}
		if got.Name != fmt.Sprintf(want, goos) {
			t.Errorf("%s: selected %q", goos, got.Name)
		}
		if !strings.Contains(logged, "same build") {
			t.Errorf("%s: logged %q; want it to say these are one build", goos, logged)
		}
	}
}

// A word that names a program rather than a variant is not resolved silently.
func TestSelectAsset_UnknownQualifierIsAmbiguous(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("tool-linux-amd64", []byte("a"))
	h.addAsset("tool-linux-amd64-server", []byte("b"))

	if got, err := h.client().SelectAsset(h.release, &Descriptor{}); err == nil {
		t.Errorf("selected %q; tool-linux-amd64-server may be a different program", got.Name)
	}
}

// Zip permits duplicate names. The member chosen from the regular files must
// be the one copied, not a symlink of the same name ahead of it.
func TestExtractBinary_ZipDuplicateNameTakesTheRegularFile(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	link := &zip.FileHeader{Name: "svc", Method: zip.Store}
	link.SetMode(os.ModeSymlink | 0o777)
	w, err := zw.CreateHeader(link)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("../../x"))
	reg := &zip.FileHeader{Name: "svc", Method: zip.Deflate}
	reg.SetMode(0o755)
	w, err = zw.CreateHeader(reg)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write([]byte("ELF"))
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}

	src := writeTemp(t, buf.Bytes(), "a.zip")
	dest := filepath.Join(t.TempDir(), "svc")
	if _, err := extractBinary(src, dest, "svc", archiveZip); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if got, _ := os.ReadFile(dest); string(got) != "ELF" {
		t.Errorf("content = %q; want the regular file, not the symlink target", got)
	}
}

// A bash completion is conventionally a file named exactly after the command.
func TestExtractBinary_CompletionWithTheBinarysNameIsNotTheBinary(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "completions/bash/svc", body: "complete -F _svc svc"},
		{name: "svc", body: "ELF", mode: 0o755},
	}), "a.tar.gz")
	dest := filepath.Join(t.TempDir(), "svc")

	member, err := extractBinary(src, dest, "svc", archiveTarGz)
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if member != "svc" {
		t.Errorf("member = %q; want the binary", member)
	}
	if got, _ := os.ReadFile(dest); string(got) != "ELF" {
		t.Errorf("content = %q", got)
	}
}

// Several members with the binary's name and no way to tell them apart is an
// error, not a first-wins guess.
func TestExtractBinary_SeveralNameMatchesAreAmbiguous(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "a/svc", body: "one", mode: 0o755},
		{name: "b/svc", body: "two", mode: 0o755},
	}), "a.tar.gz")

	_, err := extractBinary(src, filepath.Join(t.TempDir(), "svc"), "svc", archiveTarGz)
	if err == nil {
		t.Fatal("two executables named svc: want an error")
	}
	for _, want := range []string{"a/svc", "b/svc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// A binary whose name happens to begin like paperwork, or end like a man
// page, is still the binary.
func TestExtractBinary_DocLikeNamesStillMatch(t *testing.T) {
	for _, tc := range []struct {
		want    string
		entries []entry
	}{
		{"readme-server", []entry{{name: "README.md", body: "# x"}, {name: "readme-server", body: "ELF", mode: 0o755}}},
		{"changelog-bot", []entry{{name: "CHANGELOG.md", body: "# x"}, {name: "changelog-bot", body: "ELF"}}},
		{"svc", []entry{{name: "svc.1", body: "man"}, {name: "tool-1.5", body: "ELF", mode: 0o755}}},
	} {
		src := writeTemp(t, tarGz(t, tc.entries), "a.tar.gz")
		dest := filepath.Join(t.TempDir(), tc.want)
		if _, err := extractBinary(src, dest, tc.want, archiveTarGz); err != nil {
			t.Errorf("%s: extractBinary: %v", tc.want, err)
			continue
		}
		if got, _ := os.ReadFile(dest); string(got) != "ELF" {
			t.Errorf("%s: content = %q", tc.want, got)
		}
	}
}

// A Windows executable cannot run anywhere else. Off Windows it is neither a
// name match nor a fallback candidate; on Windows it is the binary.
func TestExtractBinary_ExeOnlyOnWindows(t *testing.T) {
	body := zipBytes(t, []entry{
		{name: "svc.exe", body: "PE"},
		{name: "svc-linux", body: "ELF"},
	})

	dest := filepath.Join(t.TempDir(), "svc")
	member, err := extractBinaryFor(writeTemp(t, body, "a.zip"), dest, "svc", archiveZip, "linux")
	if err != nil {
		t.Fatalf("linux: extractBinary: %v", err)
	}
	if member != "svc-linux" {
		t.Errorf("linux: member = %q; a .exe was installed on a non-Windows host", member)
	}

	member, err = extractBinaryFor(writeTemp(t, body, "a.zip"), filepath.Join(t.TempDir(), "svc"), "svc", archiveZip, "windows")
	if err != nil {
		t.Fatalf("windows: extractBinary: %v", err)
	}
	if member != "svc.exe" {
		t.Errorf("windows: member = %q", member)
	}
}

// The listing pass was unbounded; only the final copy was limited.
func TestBoundedReader_FailsPastTheLimit(t *testing.T) {
	r := &boundedReader{r: strings.NewReader(strings.Repeat("x", 100)), left: 10}
	n, err := io.Copy(io.Discard, r)
	if !errors.Is(err, errExpandedTooLarge) {
		t.Fatalf("err = %v; want errExpandedTooLarge", err)
	}
	if n != 10 {
		t.Errorf("read %d bytes past a limit of 10", n)
	}
}

// Spooling for the single pass must not leave candidates behind.
func TestExtractBinary_TarLeavesNoSpool(t *testing.T) {
	src := writeTemp(t, tarGz(t, []entry{
		{name: "svc", body: "ELF", mode: 0o755},
		{name: "helper", body: "other", mode: 0o755},
	}), "a.tar.gz")
	dir := t.TempDir()
	if _, err := extractBinary(src, filepath.Join(dir, "svc"), "svc", archiveTarGz); err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir holds %v; want only the binary", names)
	}
}

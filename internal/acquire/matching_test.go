package acquire

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// selectFrom publishes the named assets and runs SelectAsset for a platform.
func selectFrom(t *testing.T, goos, goarch string, names ...string) (Asset, error) {
	t.Helper()
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	for _, n := range names {
		h.addAsset(n, []byte(n))
	}
	c := h.client()
	c.GOOS, c.GOARCH = goos, goarch
	return c.SelectAsset(h.release, &Descriptor{})
}

// Aliases match whole words. As substrings, "mac" matched "hmac" and "win"
// matched "darwin", installing another OS's binary that cannot exec.
func TestSelectAsset_PlatformWordsAreNotSubstrings(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, asset string }{
		{"darwin", "arm64", "hmac-proxy-linux-arm64"},
		{"darwin", "amd64", "mackerel-agent_linux_amd64.tar.gz"},
		{"darwin", "arm64", "pineapple-linux-arm64"},
		{"windows", "amd64", "svc-darwin-amd64"},
		{"linux", "386", "svc-linux-x86-64"},
		{"linux", "386", "svc-linux-x86_64"},
		{"linux", "amd64", "svc-linux-amd64v3"},
	} {
		if got, err := selectFrom(t, tc.goos, tc.goarch, tc.asset); err == nil {
			t.Errorf("%s/%s selected %q", tc.goos, tc.goarch, got.Name)
		}
	}

	// And the real spellings still match, including both x86-64 forms.
	for _, tc := range []struct{ goos, goarch, asset string }{
		{"darwin", "arm64", "hmac-proxy-darwin-arm64"},
		{"darwin", "arm64", "svc-mac-arm64"},
		{"linux", "amd64", "svc-linux-x86-64"},
		{"linux", "amd64", "svc-x86_64-unknown-linux-gnu.tar.gz"},
		{"darwin", "arm64", "svc-aarch64-apple-darwin.tar.gz"},
		{"linux", "386", "svc-linux-i686"},
	} {
		if got, err := selectFrom(t, tc.goos, tc.goarch, tc.asset); err != nil || got.Name != tc.asset {
			t.Errorf("%s/%s: got %q, %v; want %q", tc.goos, tc.goarch, got.Name, err, tc.asset)
		}
	}

	// Two programs whose names merely contain an alias are not ambiguous.
	if got, err := selectFrom(t, "darwin", "arm64", "hmac-proxy-darwin-arm64", "hmac-proxy-linux-arm64"); err != nil || got.Name != "hmac-proxy-darwin-arm64" {
		t.Errorf("got %q, %v; want the darwin build", got.Name, err)
	}
}

// Files that accompany an archive are not bare binaries, and must not beat
// the archive they describe.
func TestSelectAsset_SidecarsDoNotBeatTheArchive(t *testing.T) {
	for _, sidecar := range []string{".sha512", ".md5", ".sha1", ".bundle", ".intoto.jsonl", ".cert", ".sig"} {
		got, err := selectFrom(t, "linux", "amd64", "svc_linux_amd64.tar.gz", "svc_linux_amd64.tar.gz"+sidecar)
		if err != nil || got.Name != "svc_linux_amd64.tar.gz" {
			t.Errorf("with a %s sidecar: got %q, %v; want the archive", sidecar, got.Name, err)
		}
	}
}

// Formats Outpost does not open are never installed as the binary, whatever
// they are called. Only a bare name or a supported archive is installable.
func TestSelectAsset_OnlyInstallableShapesAreConsidered(t *testing.T) {
	for _, ext := range []string{".gz", ".tar", ".xz", ".zst", ".txz", ".tbz2", ".apk", ".ipk", ".snap", ".AppImage"} {
		if got, err := selectFrom(t, "linux", "amd64", "svc-linux-amd64"+ext); err == nil {
			t.Errorf("selected %q, which Outpost cannot install", got.Name)
		}
		got, err := selectFrom(t, "linux", "amd64", "svc_1.0_linux_amd64"+ext, "svc_1.0_linux_amd64.zip")
		if err != nil || got.Name != "svc_1.0_linux_amd64.zip" {
			t.Errorf("beside a %s: got %q, %v; want the zip", ext, got.Name, err)
		}
	}

	// Version dots are not an extension, and ".bin" says it is a binary.
	for _, name := range []string{"svc_1.2.3_linux_amd64", "svc-linux-amd64-1.2", "svc-linux-amd64.bin"} {
		if got, err := selectFrom(t, "linux", "amd64", name); err != nil || got.Name != name {
			t.Errorf("got %q, %v; want %q", got.Name, err, name)
		}
	}

	// A .exe is a binary only where it can run.
	if got, err := selectFrom(t, "linux", "amd64", "svc-linux-amd64.exe"); err == nil {
		t.Errorf("linux selected %q", got.Name)
	}
	if _, err := selectFrom(t, "windows", "amd64", "svc-windows-amd64.exe"); err != nil {
		t.Errorf("windows refused its .exe: %v", err)
	}
}

func TestSelectAsset_DescriptorNamingACompressedFileIsRefused(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64.gz", []byte("x"))

	if _, err := h.client().SelectAsset(h.release, &Descriptor{Asset: "svc-${GOOS}-${GOARCH}.gz"}); err == nil {
		t.Fatal("a descriptor naming a .gz was accepted")
	}
}

// The base build is chosen on the name without its extension, so a variant in
// a shorter archive format does not become the base, and the result does not
// depend on the order the release lists its assets in.
func TestSelectAsset_PlainestIgnoresExtensionAndOrder(t *testing.T) {
	for _, order := range [][]string{
		{"app-linux-amd64.tar.gz", "app-linux-amd64-static.zip"},
		{"app-linux-amd64-static.zip", "app-linux-amd64.tar.gz"},
	} {
		if got, err := selectFrom(t, "linux", "amd64", order...); err != nil || got.Name != "app-linux-amd64.tar.gz" {
			t.Errorf("%v: got %q, %v; want the plain build", order, got.Name, err)
		}
	}
	a, errA := selectFrom(t, "linux", "amd64", "app-linux-amd64.zip", "app-linux-amd64.tar.gz")
	b, errB := selectFrom(t, "linux", "amd64", "app-linux-amd64.tar.gz", "app-linux-amd64.zip")
	if errA != nil || errB != nil || a.Name != b.Name {
		t.Errorf("the same build in two formats resolved by order: %q (%v) vs %q (%v)", a.Name, errA, b.Name, errB)
	}
}

func extractFrom(t *testing.T, body []byte, kind int, goos string, want ...string) (string, string, error) {
	t.Helper()
	ext := "a.tar.gz"
	if kind == archiveZip {
		ext = "a.zip"
	}
	src := writeTemp(t, body, ext)
	dest := filepath.Join(t.TempDir(), "out")
	member, err := extractMember(context.Background(), src, dest, kind, goos, want)
	got, _ := os.ReadFile(dest)
	return member, string(got), err
}

// Binaries whose names start like a licence or readme, or carry a version,
// are still binaries.
func TestExtractMember_DocLikeNamesAreNotThrownAway(t *testing.T) {
	for _, tc := range []struct{ service, binary string }{
		{"licensed", "licensed"},
		{"noticeboard", "noticeboard"},
		{"notice-relay", "notice-relay"},
		{"svc", "svc-1.2.5"},
	} {
		body := tarGz(t, []entry{
			{name: "LICENSE", body: "MIT"},
			{name: "LICENSE-APACHE", body: "Apache"},
			{name: tc.binary, body: "ELF", mode: 0o755},
		})
		member, got, err := extractFrom(t, body, archiveTarGz, "linux", tc.service)
		if err != nil || member != tc.binary || got != "ELF" {
			t.Errorf("%s: member %q, content %q, err %v", tc.binary, member, got, err)
		}
	}
}

// A non-executable file sharing the service's name — a completion script, a
// doc — is not the binary, whatever order the archive lists them in.
func TestExtractMember_NameMatchMustBeExecutable(t *testing.T) {
	body := tarGz(t, []entry{
		{name: "svc/completions/bash/svc", body: "complete"},
		{name: "svc/svc", body: "ELF", mode: 0o755},
	})
	if member, got, err := extractFrom(t, body, archiveTarGz, "linux", "svc"); err != nil || member != "svc/svc" || got != "ELF" {
		t.Errorf("member %q, content %q, err %v", member, got, err)
	}

	// Two executables with the service's name cannot be told apart.
	body = tarGz(t, []entry{
		{name: "a/svc", body: "one", mode: 0o755},
		{name: "b/svc", body: "two", mode: 0o755},
	})
	if member, _, err := extractFrom(t, body, archiveTarGz, "linux", "svc"); err == nil {
		t.Errorf("took %q from two executables named svc", member)
	}
}

// --name renames the service. The binary inside is still found by the name of
// the program the asset is named after.
func TestInstall_RenamedServiceStillFindsItsBinary(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("plumbline_1.2.0_linux_amd64.tar.gz", tarGz(t, []entry{
		{name: "plumbline", body: "ELF", mode: 0o755},
		{name: "plumbline-helper", body: "helper", mode: 0o755},
	}))
	c := h.client()
	asset, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "pl")
	res, err := c.Install(context.Background(), h.release, asset, dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got, _ := os.ReadFile(dest); res.Member != "plumbline" || string(got) != "ELF" {
		t.Errorf("member %q, content %q", res.Member, got)
	}
}

func TestProgramName(t *testing.T) {
	for asset, want := range map[string]string{
		"plumbline_1.2.0_linux_amd64.tar.gz": "plumbline",
		"svc-server-linux-amd64":             "svc-server",
		"yarr_darwin_arm64_gui.zip":          "yarr",
		"svc-v2.0-x86_64-unknown-linux-gnu":  "svc",
		"rust-analyzer-aarch64-apple-darwin": "rust-analyzer",
		"linux-amd64":                        "",
		"no-platform-here":                   "",
	} {
		if got := programName(asset); got != want {
			t.Errorf("programName(%q) = %q; want %q", asset, got, want)
		}
	}
}

// Nothing that cannot be the binary is installed as the sole candidate: not
// a nested archive, and not a Windows executable on Unix.
func TestExtractMember_RefusesWhatCannotRun(t *testing.T) {
	body := zipBytes(t, []entry{
		{name: "svc_linux_amd64.tar", body: "tar", mode: 0o644},
		{name: "README.md", body: "# svc"},
	})
	if member, _, err := extractFrom(t, body, archiveZip, "linux", "svc"); err == nil {
		t.Errorf("installed the nested archive %q", member)
	}

	body = zipBytes(t, []entry{{name: "svc.exe", body: "MZ", mode: 0o755}})
	_, _, err := extractFrom(t, body, archiveZip, "linux", "svc")
	if err == nil || !strings.Contains(err.Error(), "Windows") {
		t.Errorf("a lone .exe on linux: err = %v; want a refusal naming Windows", err)
	}
	if member, _, err := extractFrom(t, body, archiveZip, "windows", "svc"); err != nil || member != "svc.exe" {
		t.Errorf("windows: member %q, err %v", member, err)
	}
}

// The entry copied is the entry judged: a symlink sharing the binary's name
// is skipped by the copy as it was by the listing, and a repeated tar entry
// is an error rather than whichever copy came first.
func TestExtractMember_CopiesTheEntryItChose(t *testing.T) {
	body := zipBytes(t, []entry{
		{name: "svc", body: "/etc/passwd", mode: os.ModeSymlink | 0o777},
		{name: "svc", body: "ELF", mode: 0o755},
	})
	if _, got, err := extractFrom(t, body, archiveZip, "linux", "svc"); err != nil || got != "ELF" {
		t.Errorf("zip: content %q, err %v; want the regular file", got, err)
	}

	body = tarGz(t, []entry{
		{name: "svc", body: "OLD", mode: 0o755},
		{name: "svc", body: "NEW", mode: 0o755},
	})
	if member, _, err := extractFrom(t, body, archiveTarGz, "linux", "svc"); err == nil {
		t.Errorf("tar: took %q from two entries named svc; want an error", member)
	}
}

// Zips written on Windows record no Unix mode. Their binaries are still found.
func TestExtractMember_ZipWithoutUnixModes(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []entry{{name: "LICENSE", body: "MIT"}, {name: "svc-tool", body: "ELF"}} {
		// No SetMode: the header records a FAT creator and no permissions.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: zip.Deflate})
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
	if member, got, err := extractFrom(t, buf.Bytes(), archiveZip, "linux", "svc"); err != nil || member != "svc-tool" || got != "ELF" {
		t.Errorf("member %q, content %q, err %v", member, got, err)
	}
}

// Member names come from whoever built the archive and are printed to the
// operator's terminal; control characters must not reach it raw.
func TestPrintable(t *testing.T) {
	if got := Printable("svc_1.0/svc"); got != "svc_1.0/svc" {
		t.Errorf("a plain name was altered: %q", got)
	}
	for _, evil := range []string{"svc\x1b[1A\x1b[2K", "svc\rinstalled", "\x1b]0;x\x07svc", "svc\xff"} {
		if got := Printable(evil); strings.ContainsAny(got, "\x1b\r\x07") || strings.IndexByte(got, 0xff) >= 0 {
			t.Errorf("Printable(%q) = %q still holds control bytes", evil, got)
		}
	}

	body := tarGz(t, []entry{
		{name: "a\x1b[2Ksvc", body: "a", mode: 0o755},
		{name: "b\rsvc", body: "b", mode: 0o755},
	})
	_, _, err := extractFrom(t, body, archiveTarGz, "linux", "other")
	if err == nil || strings.ContainsAny(err.Error(), "\x1b\r") {
		t.Errorf("ambiguity error carries raw control characters: %q", err)
	}
}

// Listing decompresses the whole archive, so it is bounded in entries and by
// the caller's deadline.
func TestExtractMember_ListingIsBounded(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := 0; i <= maxArchiveEntries; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "d/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := extractFrom(t, buf.Bytes(), archiveTarGz, "linux", "svc"); err == nil || !strings.Contains(err.Error(), "entries") {
		t.Errorf("err = %v; want the entry limit", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := writeTemp(t, tarGz(t, []entry{{name: "svc", body: "ELF", mode: 0o755}}), "a.tar.gz")
	_, err := extractMember(ctx, src, filepath.Join(t.TempDir(), "svc"), archiveTarGz, "linux", []string{"svc"})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v; want the cancellation", err)
	}
}

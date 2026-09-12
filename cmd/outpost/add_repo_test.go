package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/manifest"
)

// fakeGitHub serves one release, its assets and an outpost.toml, so `outpost
// add <url>` can be exercised end to end without the network.
func fakeGitHub(t *testing.T, binary []byte, descriptor string, withChecksums bool) *httptest.Server {
	return fakeGitHubWithSum(t, binary, descriptor, withChecksums, "")
}

// fakeGitHubNamed publishes a single asset under a chosen name.
func fakeGitHubNamed(t *testing.T, binary []byte, descriptor, assetName string) *httptest.Server {
	return fakeGitHubFull(t, binary, descriptor, true, "", assetName)
}

// fakeGitHubWithSum lets a test publish a checksum that does not match the
// asset, which is the case the verification exists for.
func fakeGitHubWithSum(t *testing.T, binary []byte, descriptor string, withChecksums bool, forceSum string) *httptest.Server {
	return fakeGitHubFull(t, binary, descriptor, withChecksums, forceSum,
		fmt.Sprintf("svc-%s-%s", runtime.GOOS, runtime.GOARCH))
}

func fakeGitHubFull(t *testing.T, binary []byte, descriptor string, withChecksums bool, forceSum, assetName string) *httptest.Server {
	t.Helper()

	files := map[string][]byte{assetName: binary}
	assets := []map[string]any{}

	var srv *httptest.Server
	mux := http.NewServeMux()
	add := func(name string) {
		assets = append(assets, map[string]any{
			"name":                 name,
			"browser_download_url": srv.URL + "/dl/" + name,
			"size":                 len(files[name]),
		})
	}

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	if withChecksums {
		h := sha256.Sum256(binary)
		hash := hex.EncodeToString(h[:])
		if forceSum != "" {
			hash = forceSum
		}
		files["SHA256SUMS"] = []byte(hash + "  " + assetName + "\n")
	}
	add(assetName)
	if withChecksums {
		add("SHA256SUMS")
	}

	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1.2.3", "assets": assets})
	})
	mux.HandleFunc("/repos/o/r/contents/outpost.toml", func(w http.ResponseWriter, r *http.Request) {
		if descriptor == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(descriptor))
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	})
	return srv
}

// useFakeGitHub points the CLI's acquire client at srv for one test.
func useFakeGitHub(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := acquireClient
	t.Cleanup(func() { acquireClient = prev })
	acquireClient = func() *acquire.Client {
		return &acquire.Client{
			HTTP:    srv.Client(),
			BaseURL: srv.URL,
			GOOS:    runtime.GOOS,
			GOARCH:  runtime.GOARCH,
		}
	}
}

const helloDescriptor = `
name        = "hello"
args        = ["serve", "--port", "${PORT}"]
env         = { LOG_LEVEL = "debug" }
healthcheck = "/healthz"
mcp         = true
start_grace_seconds = 15
`

// The headline case: a URL is enough to install a service and write a correct
// manifest, with no --binary and no hand-written args.
func TestAdd_FromGitHubURL(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 0\n"), helloDescriptor, true))

	stdout, stderr, err := runCLI(t, home, "add", "https://github.com/o/r")
	if err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}

	body, err := os.ReadFile(filepath.Join(home, "services", "hello.toml"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	manifest := string(body)

	for _, want := range []string{
		`name           = "hello"`,
		`healthcheck    = "/healthz"`,
		`mcp            = true`,
		"start_grace_seconds = 15",
		`"serve"`,
		`LOG_LEVEL = "debug"`,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest missing %q:\n%s", want, manifest)
		}
	}

	// The descriptor's ${PORT} must have become the port Outpost assigned,
	// otherwise the service binds something other than what Outpost probes.
	port := manifestPort(t, manifest)
	if !strings.Contains(manifest, fmt.Sprintf(`"%d"`, port)) {
		t.Errorf("args do not carry the assigned port %d:\n%s", port, manifest)
	}

	installed := filepath.Join(home, "bin", "hello")
	fi, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed binary is not executable (mode %#o)", fi.Mode().Perm())
	}
	if !strings.Contains(stdout, "checksum verified") {
		t.Errorf("stdout does not report verification:\n%s", stdout)
	}
}

// Without a descriptor the flags still have to work, and the operator must be
// told they are getting defaults rather than the service's own configuration.
func TestAdd_FromGitHubURLWithoutADescriptor(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), "", true))

	_, stderr, err := runCLI(t, home, "add", "https://github.com/o/r",
		"--name", "custom", "--args", "run,--verbose")
	if err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "publishes no outpost.toml") {
		t.Errorf("stderr does not mention the missing descriptor:\n%s", stderr)
	}

	body, err := os.ReadFile(filepath.Join(home, "services", "custom.toml"))
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if !strings.Contains(string(body), `"--verbose"`) {
		t.Errorf("flags were not applied:\n%s", body)
	}
}

// An explicit flag beats the descriptor; the operator asked for it by name.
func TestAdd_FlagsOverrideTheDescriptor(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), helloDescriptor, true))

	if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r",
		"--healthcheck", "/ready", "--restart", "always"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}

	body, _ := os.ReadFile(filepath.Join(home, "services", "hello.toml"))
	if !strings.Contains(string(body), `healthcheck    = "/ready"`) {
		t.Errorf("flag did not override the descriptor:\n%s", body)
	}
	if !strings.Contains(string(body), `restart_policy = "always"`) {
		t.Errorf("flag did not override the default:\n%s", body)
	}
}

// A corrupted or substituted asset must stop the install, and must not leave a
// half-installed service behind.
func TestAdd_ChecksumMismatchInstallsNothing(t *testing.T) {
	home := t.TempDir()
	// A checksum that does not match the asset, as a substituted binary would
	// produce.
	useFakeGitHub(t, fakeGitHubWithSum(t, []byte("binary"), helloDescriptor, true, strings.Repeat("0", 64)))

	_, _, err := runCLI(t, home, "add", "https://github.com/o/r")
	if err == nil {
		t.Fatal("checksum mismatch: want an error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v; want it to name the mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "services", "hello.toml")); !os.IsNotExist(statErr) {
		t.Error("a manifest was written despite the mismatch")
	}
	if _, statErr := os.Stat(filepath.Join(home, "bin", "hello")); !os.IsNotExist(statErr) {
		t.Error("a binary was installed despite the mismatch")
	}
}

// A service name is a filename; a repository must not be able to choose one
// that escapes the services directory.
func TestValidServiceName_RejectsPathTricks(t *testing.T) {
	for _, name := range []string{"", ".", "..", "a/b", ".hidden", "../escape"} {
		if err := validServiceName(name); err == nil {
			t.Errorf("validServiceName(%q) = nil; want an error", name)
		}
	}
	if err := validServiceName("plumbline"); err != nil {
		t.Errorf("validServiceName(\"plumbline\") = %v; want nil", err)
	}
}

func TestIsRepoRef(t *testing.T) {
	for _, ref := range []string{"https://github.com/o/r", "github.com/o/r", "o/r", "git@github.com:o/r.git"} {
		if !isRepoRef(ref) {
			t.Errorf("isRepoRef(%q) = false; want true", ref)
		}
	}
	for _, name := range []string{"plumbline", "kosync", "web-1"} {
		if isRepoRef(name) {
			t.Errorf("isRepoRef(%q) = true; want false for a service name", name)
		}
	}
}

// manifestPort digs the assigned port out of a rendered manifest.
func manifestPort(t *testing.T, body string) int {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), "port"); ok {
			var p int
			if _, err := fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after), "=")), "%d", &p); err == nil {
				return p
			}
		}
	}
	t.Fatalf("no port in manifest:\n%s", body)
	return 0
}

// A manifest the declared set rejects must leave nothing behind. Installing
// first and validating second meant a failed add still wrote a binary — and
// with --force, overwrote a running service's binary before aborting, so the
// daemon would restart it into the new binary under the old manifest.
func TestAdd_ValidationFailureInstallsNothing(t *testing.T) {
	home := t.TempDir()
	// A descriptor naming a port Outpost did not assign: portInArgs rejects
	// the manifest, because otherwise the service binds one port while Outpost
	// health-checks another and restart-loops it forever.
	const mismatched = `
name = "hello"
args = ["serve", "--port", "8080"]
`
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), mismatched, true))

	_, _, err := runCLI(t, home, "add", "https://github.com/o/r")
	if err == nil {
		t.Fatal("a manifest with a port mismatch was accepted")
	}

	if _, statErr := os.Stat(filepath.Join(home, "bin", "hello")); !os.IsNotExist(statErr) {
		t.Error("a binary was installed for a service whose manifest was rejected")
	}
	if _, statErr := os.Stat(filepath.Join(home, "services", "hello.toml")); !os.IsNotExist(statErr) {
		t.Error("a manifest was written despite validation failing")
	}
}

// --binary alongside a repository reference used to be ignored silently, so
// the operator's local build was discarded for a downloaded one.
func TestAdd_BinaryWithRepoRefIsRefused(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), helloDescriptor, true))

	_, _, err := runCLI(t, home, "add", "https://github.com/o/r", "--binary", "/bin/sleep")
	if err == nil {
		t.Fatal("--binary with a repository reference was accepted")
	}
	if !strings.Contains(err.Error(), "--binary cannot be combined") {
		t.Errorf("error = %v; want it to explain the conflict", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "bin", "hello")); !os.IsNotExist(statErr) {
		t.Error("a binary was downloaded despite the conflicting flags")
	}
}

// The service spec has told authors to support OUTPOST_PORT since §1 was
// written. Until it was actually set, a service that took that advice and
// shipped no args got no port at all.
func TestAdd_GeneratedManifestSetsOutpostPort(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), "name = \"hello\"\n", true))

	if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	body, err := os.ReadFile(filepath.Join(home, "services", "hello.toml"))
	if err != nil {
		t.Fatal(err)
	}
	port := manifestPort(t, string(body))
	if want := fmt.Sprintf(`OUTPOST_PORT = "%d"`, port); !strings.Contains(string(body), want) {
		t.Errorf("manifest does not set %s:\n%s", want, body)
	}
}

// A descriptor that sets OUTPOST_PORT itself said so deliberately.
func TestAdd_DescriptorOutpostPortIsNotOverwritten(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"),
		"name = \"hello\"\nenv = { OUTPOST_PORT = \"fixed\" }\n", true))

	if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	body, _ := os.ReadFile(filepath.Join(home, "services", "hello.toml"))
	if !strings.Contains(string(body), `OUTPOST_PORT = "fixed"`) {
		t.Errorf("the descriptor's own OUTPOST_PORT was overwritten:\n%s", body)
	}
}

// --print must emit a manifest and nothing else, so it can be redirected into
// a file the way the --binary path's can. Progress belongs on stderr.
func TestAdd_PrintEmitsOnlyTheManifest(t *testing.T) {
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), helloDescriptor, true))

	stdout, _, err := runCLI(t, t.TempDir(), "add", "https://github.com/o/r", "--print")
	if err != nil {
		t.Fatalf("add --print: %v", err)
	}
	if strings.Contains(stdout, "looking for") || strings.Contains(stdout, "would install") {
		t.Errorf("progress leaked onto stdout:\n%s", stdout)
	}

	path := filepath.Join(t.TempDir(), "printed.toml")
	if err := os.WriteFile(path, []byte(stdout), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manifest.Load(path); err != nil {
		t.Errorf("`--print > file` does not parse as a manifest: %v\n%s", err, stdout)
	}
}

// The daemon runs from launchd's working directory, not the operator's, so a
// relative --home must not leak a relative binary path into the manifest.
func TestAdd_BinaryPathIsAbsoluteWithARelativeHome(t *testing.T) {
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"), helloDescriptor, true))

	parent := t.TempDir()
	t.Chdir(parent)

	if _, stderr, err := runCLI(t, "./op", "add", "https://github.com/o/r"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}

	svc, err := manifest.Load(filepath.Join(parent, "op", "services", "hello.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(svc.Binary) {
		t.Errorf("binary = %q; want an absolute path", svc.Binary)
	}
}

// OUTPOST_PORT is how a service is told its port, so pinning it names the
// port. Allocating a different one instead guaranteed the very validation
// error that mismatch exists to raise, which made the flag unusable.
func TestAdd_EnvOutpostPortSettlesThePort(t *testing.T) {
	home := t.TempDir()
	if _, stderr, err := runCLI(t, home, "add", "svc",
		"--binary", "/bin/ls", "--env", "OUTPOST_PORT=8731"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "svc.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Port != 8731 {
		t.Errorf("port = %d; want the pinned 8731", svc.Port)
	}
}

// The same for a descriptor: pinning a numeric OUTPOST_PORT made the whole
// repository uninstallable, and only went unnoticed because the first test
// written for it used a non-numeric value.
func TestAdd_DescriptorOutpostPortSettlesThePort(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("binary"),
		"name = \"hello\"\nenv = { OUTPOST_PORT = \"8732\" }\n", true))

	if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r"); err != nil {
		t.Fatalf("add: %v\nstderr: %s", err, stderr)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "hello.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Port != 8732 {
		t.Errorf("port = %d; want the descriptor's pinned 8732", svc.Port)
	}
}

// An operator naming both, differently, is a genuine disagreement and must
// still be refused — with the recourse spelled out.
func TestAdd_ExplicitPortDisagreeingWithEnvIsStillRefused(t *testing.T) {
	_, _, err := runCLI(t, t.TempDir(), "add", "svc",
		"--binary", "/bin/ls", "--env", "OUTPOST_PORT=8731", "--port", "9000")
	if err == nil {
		t.Fatal("a port disagreeing with OUTPOST_PORT was accepted")
	}
	if !strings.Contains(err.Error(), "pass --port 8731") {
		t.Errorf("error = %v; want it to name the recourse", err)
	}
}

// helloZip is the shape most real releases use: the binary inside an archive,
// with the usual clutter beside it. yarr, the first repository this was tried
// against, ships exactly this and nothing else.
func helloZip(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range []struct {
		name string
		body string
		mode os.FileMode
	}{
		{"LICENSE", "MIT", 0o644},
		{"README.md", "# hello", 0o644},
		{"hello", body, 0o755},
	} {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode)
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

// The case that started this: a release shipping only archives used to fail
// with "publishes no asset for <platform>", which was not even true.
func TestAdd_FromAReleaseThatShipsOnlyArchives(t *testing.T) {
	home := t.TempDir()
	asset := fmt.Sprintf("hello_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	useFakeGitHub(t, fakeGitHubNamed(t, helloZip(t, "#!/bin/sh\nexit 0\n"), "name = \"hello\"\n", asset))

	stdout, stderr, err := runCLI(t, home, "add", "https://github.com/o/r")
	if err != nil {
		t.Fatalf("add from an archive release: %v\nstderr: %s", err, stderr)
	}

	installed := filepath.Join(home, "bin", "hello")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("binary not installed: %v", err)
	}
	if string(got) != "#!/bin/sh\nexit 0\n" {
		t.Errorf("installed the wrong file: %q", got)
	}
	fi, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("installed binary is not executable (%#o)", fi.Mode().Perm())
	}
	if !strings.Contains(stdout, "unpacked hello from "+asset) {
		t.Errorf("stdout does not report the unpacking:\n%s", stdout)
	}
	// The archive is what the release checksummed, so verification still holds.
	if !strings.Contains(stdout, "checksum verified") {
		t.Errorf("stdout does not report verification:\n%s", stdout)
	}
	if _, err := manifest.Load(filepath.Join(home, "services", "hello.toml")); err != nil {
		t.Errorf("manifest: %v", err)
	}
}

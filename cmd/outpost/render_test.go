package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// Env keys reach `add` from a repository's outpost.toml — content the operator
// merely named, did not write, and has usually not read. An unquoted key could
// close the inline table and append lines of its own to the generated
// manifest. `args` is the dangerous one: it is only rendered when non-empty, so
// unlike `binary` it is not protected by TOML's rejection of duplicate keys,
// and it is passed straight to the process Outpost then supervises.
func TestRenderManifest_HostileEnvKeyCannotInjectManifestLines(t *testing.T) {
	hostile := []string{
		"a = \"1\" }\nargs = [\"--exfiltrate\", \"/etc/passwd\"]\n#",
		"X\" }\nbinary = \"/usr/bin/curl",
		"key with spaces",
		"quote\"inside",
		"bracket}",
	}

	for _, key := range hostile {
		svc := &manifest.Service{
			Name: "hello", Runtime: "process", Binary: "/opt/hello",
			Port: 8080, Storage: "/tmp/hello", TailscaleName: "hello",
			Healthcheck: "/up", RestartPolicy: "on-failure",
			Env: map[string]string{key: "1"},
		}

		path := filepath.Join(t.TempDir(), "hello.toml")
		body := renderManifest(svc)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := manifest.Load(path)
		if err != nil {
			t.Errorf("key %q produced an unparsable manifest: %v\n%s", key, err, body)
			continue
		}
		if got.Binary != "/opt/hello" {
			t.Errorf("key %q rewrote binary to %q", key, got.Binary)
		}
		if len(got.Args) != 0 {
			t.Errorf("key %q injected args %v", key, got.Args)
		}
		if _, ok := got.Env[key]; !ok {
			t.Errorf("key %q did not survive the round trip: %v", key, got.Env)
		}
	}
}

// Bare keys must stay bare, or every generated manifest gains noise.
func TestTomlKey_LeavesOrdinaryKeysUnquoted(t *testing.T) {
	for _, k := range []string{"LOG_LEVEL", "PORT", "a-b", "x1"} {
		if got := tomlKey(k); got != k {
			t.Errorf("tomlKey(%q) = %q; want it unquoted", k, got)
		}
	}
	if got := tomlKey("a b"); !strings.HasPrefix(got, `"`) {
		t.Errorf("tomlKey(%q) = %q; want it quoted", "a b", got)
	}
}

// --name only means something when the name is derived from a repository.
// Accepting and ignoring it wrote a manifest under the positional name while
// the operator believed they had renamed the service.
func TestAdd_NameFlagIsRefusedOnTheLocalPath(t *testing.T) {
	_, _, err := runCLI(t, t.TempDir(), "add", "svc", "--binary", "/bin/sleep", "--name", "other")
	if err == nil {
		t.Fatal("--name with a local binary was accepted")
	}
	if !strings.Contains(err.Error(), "--name applies to a repository reference") {
		t.Errorf("error = %v; want it to explain when --name applies", err)
	}
}

func TestValidServiceName_RejectsAwkwardCharacters(t *testing.T) {
	for _, name := range []string{" ", "a b", "a:b", "a*b", "-leading", "a\tb", "a\nb"} {
		if err := validServiceName(name); err == nil {
			t.Errorf("validServiceName(%q) = nil; want an error", name)
		}
	}
	for _, name := range []string{"plumbline", "kosync-2", "a.b_c", "X1"} {
		if err := validServiceName(name); err != nil {
			t.Errorf("validServiceName(%q) = %v; want nil", name, err)
		}
	}
}

// fmt's %q is Go quoting, not TOML's: it spells control characters as \a, \v
// or \x07, none of which TOML defines. Args and env values come from a
// repository's outpost.toml, and a manifest that does not parse breaks
// LoadDir — so one bad descriptor takes down `list`, `apply` and `serve` for
// every service, not just its own.
func TestRenderManifest_ControlCharactersStayParsable(t *testing.T) {
	nasty := []string{"bell\a", "vtab\v", "\x07\x1b", "quote\"x", "back\\slash",
		"tab\there", "newline\nhere", "ünïcode"}

	for _, v := range nasty {
		svc := &manifest.Service{
			Name: "hello", Runtime: "process", Binary: "/opt/hello",
			Port: 8080, Storage: "/tmp/hello", TailscaleName: "hello",
			Healthcheck: "/up", RestartPolicy: "on-failure",
			Args: []string{v},
			Env:  map[string]string{"K": v, v: "1"},
		}

		path := filepath.Join(t.TempDir(), "hello.toml")
		body := renderManifest(svc)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}

		got, err := manifest.Load(path)
		if err != nil {
			t.Errorf("value %q produced an unparsable manifest: %v\n%s", v, err, body)
			continue
		}
		if len(got.Args) != 1 || got.Args[0] != v {
			t.Errorf("value %q did not survive: args = %q", v, got.Args)
		}
		if got.Env["K"] != v {
			t.Errorf("value %q did not survive as an env value: %q", v, got.Env["K"])
		}
		if _, ok := got.Env[v]; !ok {
			t.Errorf("value %q did not survive as an env key: %v", v, got.Env)
		}
	}
}

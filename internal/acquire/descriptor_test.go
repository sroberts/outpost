package acquire

import (
	"strings"
	"testing"
)

func TestParseDescriptor_ReadsTheFields(t *testing.T) {
	d, err := ParseDescriptor([]byte(`
name        = "plumbline"
port        = 9000
args        = ["serve", "--port", "${PORT}"]
env         = { LOG_LEVEL = "info", ADDR = "127.0.0.1:${PORT}" }
healthcheck = "/healthz"
mcp         = true
restart_policy = "always"
start_grace_seconds = 30
asset       = "plumbline-${GOOS}-${GOARCH}"
`))
	if err != nil {
		t.Fatalf("ParseDescriptor: %v", err)
	}
	if d.Name != "plumbline" || d.Port != 9000 || d.Healthcheck != "/healthz" {
		t.Errorf("unexpected descriptor: %+v", d)
	}
	if !d.MCP || d.RestartPolicy != "always" || d.StartGraceSeconds != 30 {
		t.Errorf("unexpected descriptor: %+v", d)
	}
}

// Outpost picks the port, so the service has to be told what it got. Without
// this substitution a descriptor cannot express its own command line.
func TestDescriptor_ExpandSubstitutesThePort(t *testing.T) {
	d := &Descriptor{
		Args: []string{"serve", "--port", "${PORT}"},
		Env:  map[string]string{"ADDR": "127.0.0.1:${PORT}", "PLAIN": "x"},
	}
	args, env := d.Expand(8731)

	if got := strings.Join(args, " "); got != "serve --port 8731" {
		t.Errorf("args = %q; want the port substituted", got)
	}
	if env["ADDR"] != "127.0.0.1:8731" {
		t.Errorf("env ADDR = %q; want the port substituted", env["ADDR"])
	}
	if env["PLAIN"] != "x" {
		t.Errorf("env PLAIN = %q; want it untouched", env["PLAIN"])
	}
	// Expand must not rewrite the descriptor it read from.
	if d.Args[2] != "${PORT}" {
		t.Errorf("Expand mutated the descriptor: %q", d.Args[2])
	}
}

func TestDescriptor_AssetTemplate(t *testing.T) {
	d := &Descriptor{Asset: "svc-${GOOS}-${GOARCH}"}
	if got := d.assetName("darwin", "arm64"); got != "svc-darwin-arm64" {
		t.Errorf("assetName = %q", got)
	}
	if got := (&Descriptor{}).assetName("linux", "amd64"); got != "" {
		t.Errorf("assetName without a template = %q; want empty", got)
	}
	// A nil descriptor is the no-descriptor case and must not panic.
	var nilDesc *Descriptor
	if got := nilDesc.assetName("linux", "amd64"); got != "" {
		t.Errorf("assetName on nil = %q; want empty", got)
	}
}

func TestParseDescriptor_RejectsNonsense(t *testing.T) {
	for name, body := range map[string]string{
		"malformed toml":  `name = "unclosed`,
		"port too high":   `port = 99999`,
		"negative port":   `port = -1`,
		"relative health": `healthcheck = "up"`,
		"bad policy":      `restart_policy = "sometimes"`,
	} {
		if _, err := ParseDescriptor([]byte(body)); err == nil {
			t.Errorf("%s: want an error, got nil", name)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	manifest := "abc123  svc-linux-amd64\ndef456 *svc-darwin-arm64\n789xyz  dist/svc-linux-arm64\n"
	for _, tc := range []struct{ asset, want string }{
		{"svc-linux-amd64", "abc123"},
		{"svc-darwin-arm64", "def456"}, // sha256sum's binary-mode "*" prefix
		{"svc-linux-arm64", "789xyz"},  // a path rather than a bare name
		{"not-published", ""},
	} {
		if got := parseChecksums(manifest, tc.asset); got != tc.want {
			t.Errorf("parseChecksums(%q) = %q; want %q", tc.asset, got, tc.want)
		}
	}

	// A "<asset>.sha256" usually holds the bare hash and nothing else.
	if got := parseChecksums("abc123\n", "svc-linux-amd64"); got != "abc123" {
		t.Errorf("lone hash = %q; want abc123", got)
	}
	// But a multi-line file with no matching name must not fall back to one.
	if got := parseChecksums("abc123  other\ndef456  another\n", "svc-linux-amd64"); got != "" {
		t.Errorf("got %q; want no match", got)
	}
}

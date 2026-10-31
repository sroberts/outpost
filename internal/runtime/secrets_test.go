package runtime

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// A service may be a third-party binary; it must not inherit what lets the
// daemon join nodes to the tailnet or act on GitHub.
func TestBuildEnv_DropsTheDaemonsCredentials(t *testing.T) {
	for _, k := range []string{"TS_AUTHKEY", "TS_AUTH_KEY", "GITHUB_TOKEN", "GH_TOKEN", "OUTPOST_TSNET_AUTHKEY"} {
		t.Setenv(k, "secret-"+k)
	}
	t.Setenv("OUTPOST_TEST_KEPT", "1")
	svc := &manifest.Service{
		Name:    "alpha",
		Storage: t.TempDir(),
		Env:     map[string]string{"APP_MODE": "x"},
	}
	env := buildEnv(svc, nil)
	for _, kv := range env {
		if strings.Contains(kv, "secret-") {
			t.Errorf("a service inherited %q", kv)
		}
	}
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{"\nOUTPOST_TEST_KEPT=1\n", "\nAPP_MODE=x\n"} {
		if !strings.Contains(joined, want) {
			t.Errorf("env lost %q", strings.TrimSpace(want))
		}
	}
}

// A key the manifest sets on purpose is the operator's choice, not leakage.
func TestBuildEnv_ManifestCanStillSetACredential(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "daemon-token")
	svc := &manifest.Service{
		Name:    "alpha",
		Storage: t.TempDir(),
		Env:     map[string]string{"GITHUB_TOKEN": "service-token"},
	}
	var got []string
	for _, kv := range buildEnv(svc, nil) {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			got = append(got, kv)
		}
	}
	if len(got) != 1 || got[0] != "GITHUB_TOKEN=service-token" {
		t.Errorf("GITHUB_TOKEN entries = %q; want only the manifest's", got)
	}
}

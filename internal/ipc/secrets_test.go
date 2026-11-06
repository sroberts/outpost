package ipc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/secrets"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// secretsDaemon serves the secret.* commands over a supervisor that has the
// given services declared but never runs its loop, so nothing starts.
func secretsDaemon(t *testing.T, withStore bool, services ...string) (*Client, *secrets.Store, *logBuffer) {
	t.Helper()
	home := t.TempDir()
	svcDir := filepath.Join(home, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, name := range services {
		body := fmt.Sprintf("name = %q\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nport = %d\nstorage = %q\ntailscale_name = %q\n",
			name, 19200+i, filepath.Join(home, "data", name), name)
		if err := os.WriteFile(filepath.Join(svcDir, name+".toml"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir:    svcDir,
		Store:          st,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}

	logs := &logBuffer{}
	sock := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sock, sup, logs.logf)
	if err != nil {
		t.Fatal(err)
	}
	var sec *secrets.Store
	if withStore {
		sec, err = secrets.Open(filepath.Join(home, "secrets.db"), filepath.Join(home, "secrets.key"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sec.Close() })
		srv.UseSecrets(sec)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sock), sec, logs
}

func TestSecret_SetBindRotateRemove(t *testing.T) {
	c, store, logs := secretsDaemon(t, true, "sous", "yarr")
	const value, rotated = "sk-distinctive-value-1", "sk-distinctive-value-2"

	msg, err := c.SecretSet("openai-key", "", value)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "created") || !strings.Contains(msg, "OPENAI_KEY") {
		t.Errorf("create said %q; want it created as OPENAI_KEY", msg)
	}

	if msg, err = c.SecretBind("openai-key", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "restarting sous, yarr with it") {
		t.Errorf("global bind said %q", msg)
	}

	if msg, err = c.SecretSet("openai-key", "", rotated); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "rotated") {
		t.Errorf("second set said %q; want a rotation", msg)
	}
	if vars, _ := store.Resolve("sous"); vars["OPENAI_KEY"] != rotated {
		t.Errorf("sous gets %q after rotating", vars["OPENAI_KEY"])
	}
	if _, err := c.SecretSet("openai-key", "OTHER", "x"); err == nil || !strings.Contains(err.Error(), "remove it and set it again") {
		t.Errorf("changing the variable on rotate: %v", err)
	}

	list, err := c.SecretList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Global || list[0].Revision != 2 || strings.Join(list[0].Reaches, ",") != "sous,yarr" {
		t.Errorf("list = %+v", list)
	}

	if msg, err = c.SecretUnbind("openai-key", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "restarting sous, yarr without it") {
		t.Errorf("unbind said %q", msg)
	}
	if _, err = c.SecretBind("openai-key", "yarr"); err != nil {
		t.Fatal(err)
	}
	if msg, err = c.SecretRemove("openai-key"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "removed; restarting yarr without it") {
		t.Errorf("remove said %q", msg)
	}
	if list, _ := c.SecretList(); len(list) != 0 {
		t.Errorf("still listed after removal: %+v", list)
	}

	if strings.Contains(logs.String(), "distinctive") {
		t.Errorf("a value reached the daemon log:\n%s", logs.String())
	}
}

func TestSecret_Refusals(t *testing.T) {
	c, _, _ := secretsDaemon(t, true, "sous")
	if _, err := c.SecretSet("", "", "v"); err == nil {
		t.Error("set with no name")
	}
	if _, err := c.SecretSet("k", "", ""); err == nil {
		t.Error("set with an empty value")
	}
	if _, err := c.SecretSet("k", "OUTPOST_PORT", "v"); err == nil {
		t.Error("set an OUTPOST_ variable")
	}
	if _, err := c.SecretBind("missing", "sous"); err == nil {
		t.Error("bound a secret that doesn't exist")
	}
	if _, err := c.SecretSet("k", "", "v"); err != nil {
		t.Fatal(err)
	}
	// Neither a service nor --global, through the raw request.
	if _, err := c.do(Request{Command: CmdSecretBind, Secret: &SecretRequest{Name: "k"}}); err == nil {
		t.Error("bound to nothing")
	}
	if _, err := c.do(Request{Command: CmdSecretBind, Secret: &SecretRequest{Name: "k", Service: "sous", Global: true}}); err == nil {
		t.Error("bound to a service and every service at once")
	}
}

func TestSecret_WithoutStoreSaysSo(t *testing.T) {
	c, _, _ := secretsDaemon(t, false)
	if _, err := c.SecretList(); err == nil || !strings.Contains(err.Error(), "without a secrets store") {
		t.Errorf("list without a store: %v", err)
	}
}

func TestDefaultEnv(t *testing.T) {
	for in, want := range map[string]string{
		"openai-key":  "OPENAI_KEY",
		"stripe.live": "STRIPE_LIVE",
		"gh_token2":   "GH_TOKEN2",
	} {
		if got := secrets.DefaultEnv(in); got != want {
			t.Errorf("DefaultEnv(%q) = %q, want %q", in, got, want)
		}
	}
}

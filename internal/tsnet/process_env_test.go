package tsnet

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestHelperNodeEnv is not a test. It is a fake node that reports the
// credentials it was started with.
func TestHelperNodeEnv(t *testing.T) {
	if os.Getenv("OUTPOST_HELPER_ENV_NODE") != "1" {
		return
	}
	fmt.Fprintf(os.Stderr, "node-key=%s\n", os.Getenv(AuthKeyEnv))
	fmt.Fprintf(os.Stderr, "ts-authkey=%q github=%q\n", os.Getenv("TS_AUTHKEY"), os.Getenv("GITHUB_TOKEN"))
	fmt.Println(ReadyLine)
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// The node gets the key it is handed and nothing else of the daemon's
// credentials, even when the environment nodeCmd built carries them.
func TestProcessNode_EnvCarriesOnlyItsOwnKey(t *testing.T) {
	t.Setenv("TS_AUTHKEY", "tskey-ambient")
	t.Setenv("GITHUB_TOKEN", "ghp-ambient")
	t.Setenv(AuthKeyEnv, "tskey-stale")

	logs := &logSink{}
	r, err := New(Options{StateDir: t.TempDir(), AuthKey: "tskey-handed", Logf: logs.logf})
	if err != nil {
		t.Fatal(err)
	}
	r.nodeCmd = func(cfg nodeConfig) (*exec.Cmd, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperNodeEnv$")
		cmd.Env = append(os.Environ(), "OUTPOST_HELPER_ENV_NODE=1")
		return cmd, nil
	}
	r.retryInitial = 10 * time.Millisecond
	r.retryMax = 50 * time.Millisecond
	r.nodeStopGrace = 2 * time.Second
	t.Cleanup(func() { _ = r.Close() })

	if err := r.Attach(context.Background(), svc("scrubbed", 9207)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "the node's environment to be logged", func() bool {
		return logs.has("node-key=") && logs.has("ts-authkey=")
	})

	if !logs.has("node-key=tskey-handed") || logs.has("tskey-stale") {
		t.Errorf("the node did not receive just the key it was handed: %q", logs.snapshot())
	}
	if !logs.has(`ts-authkey="" github=""`) {
		t.Errorf("the node inherited the daemon's credentials: %q", logs.snapshot())
	}
}

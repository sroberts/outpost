package tsnet

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestHelperHostedNodeProcess is not a test. It is a fake node that reports
// the DNS name it was given, as the real one does, then its arguments.
func TestHelperHostedNodeProcess(t *testing.T) {
	if os.Getenv("OUTPOST_HELPER_HOSTED_NODE") != "1" {
		return
	}
	// The trailing dot is how Tailscale reports it.
	fmt.Println(HostLinePrefix + "yarr-1.example.ts.net.")
	fmt.Println(ReadyLine)
	fmt.Fprintln(os.Stderr, "args="+strings.Join(os.Args, " "))
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// A node's real name can differ from the one asked for, and the gateway has
// to serve the one browsers will use.
func TestProcessNode_ReportsItsHostAndGetsTheUpstream(t *testing.T) {
	logs := &logSink{}
	r, err := New(Options{
		StateDir: t.TempDir(),
		Logf:     logs.logf,
		Upstream: "http://127.0.0.1:8443",
	})
	if err != nil {
		t.Fatal(err)
	}
	r.nodeCmd = func(cfg nodeConfig) (*exec.Cmd, error) {
		args := append([]string{"-test.run=^TestHelperHostedNodeProcess$", "--"}, nodeArgs(cfg)...)
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), "OUTPOST_HELPER_HOSTED_NODE=1")
		return cmd, nil
	}
	r.nodeStopGrace = 2 * time.Second
	t.Cleanup(func() { _ = r.Close() })

	if err := r.Attach(context.Background(), svc("yarr", 9301)); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, 10*time.Second, "the node's host to be known", func() bool {
		return r.Hosts()["yarr"] == "yarr-1.example.ts.net"
	})
	waitFor(t, 5*time.Second, "the node's arguments to be logged", func() bool {
		return logs.has("--upstream http://127.0.0.1:8443")
	})
	if !logs.has("https://yarr-1.example.ts.net -> gateway") {
		t.Errorf("attach did not log where the service is: %q", logs.snapshot())
	}

	if err := r.Detach(context.Background(), "yarr"); err != nil {
		t.Fatal(err)
	}
	if h := r.Hosts(); len(h) != 0 {
		t.Errorf("a detached service still has a host: %v", h)
	}
}

// In front of the gateway, the node must say which tailnet name was asked for
// and that it was asked over HTTPS, or the gateway can't route or secure it.
func TestUpstreamProxy_CarriesHostAndScheme(t *testing.T) {
	var got *http.Request
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(context.Background())
	}))
	t.Cleanup(gw.Close)
	target, _ := url.Parse(gw.URL)

	front := httptest.NewTLSServer(newProxyTo(target, ""))
	t.Cleanup(front.Close)
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/feeds", nil)
	req.Host = "yarr-1.example.ts.net"
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if got == nil {
		t.Fatal("the request never reached the upstream")
	}
	if got.Host != "yarr-1.example.ts.net" {
		t.Errorf("Host = %q", got.Host)
	}
	if p := got.Header.Get("X-Forwarded-Proto"); p != "https" {
		t.Errorf("X-Forwarded-Proto = %q; want https", p)
	}
	if h := got.Header.Get("X-Forwarded-Host"); h != "yarr-1.example.ts.net" {
		t.Errorf("X-Forwarded-Host = %q", h)
	}
}

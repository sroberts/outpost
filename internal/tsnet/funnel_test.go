package tsnet

import (
	"context"
	"encoding/json"
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

	"github.com/sroberts/outpost/internal/manifest"
)

func TestNodeArgs_CarryFunnel(t *testing.T) {
	args := strings.Join(nodeArgs(nodeConfig{Service: "a", Funnel: true, PublicUpstream: "http://127.0.0.1:9"}), " ")
	if !strings.Contains(args, "--funnel --public-upstream http://127.0.0.1:9") {
		t.Errorf("args = %s", args)
	}
	if strings.Contains(strings.Join(nodeArgs(nodeConfig{Service: "a"}), " "), "funnel") {
		t.Error("a node without funnel was started with it")
	}
}

// What arrives through Funnel reaches the public upstream for the node's own
// name only, with the client's address as the only forwarded one, and
// never as an upgrade.
func TestFunnelHandler(t *testing.T) {
	type got struct {
		Host, XFF, Proto, Path, Body string
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(got{r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("X-Forwarded-Proto"), r.URL.Path, string(b)})
	}))
	t.Cleanup(up.Close)
	target, _ := url.Parse(up.URL)
	h := NewFunnelHandler("tasking.tail.ts.net.", target, "")

	send := func(host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "https://"+host+"/webhooks/github", strings.NewReader(body))
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		r = r.WithContext(WithFunnelClient(r.Context(), "203.0.113.7"))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	w := send("tasking.tail.ts.net", map[string]string{"X-Forwarded-For": "10.0.0.1"}, `{"zen":"ok"}`)
	var g got
	if err := json.Unmarshal(w.Body.Bytes(), &g); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if g.Host != "tasking.tail.ts.net" || g.XFF != "203.0.113.7" || g.Proto != "https" || g.Path != "/webhooks/github" || g.Body != `{"zen":"ok"}` {
		t.Errorf("upstream saw %+v", g)
	}

	if w := send("outpost.tail.ts.net", nil, ""); w.Code != http.StatusNotFound {
		t.Errorf("another name: %d", w.Code)
	}
	for _, hdr := range []map[string]string{{"Upgrade": "websocket"}, {"Connection": "keep-alive, Upgrade"}} {
		if w := send("tasking.tail.ts.net", hdr, ""); w.Code != http.StatusBadRequest {
			t.Errorf("upgrade %v: %d", hdr, w.Code)
		}
	}
	// Until the node knows its name, nothing matches it.
	r := httptest.NewRequest(http.MethodGet, "https://tasking.tail.ts.net/", nil)
	nw := httptest.NewRecorder()
	NewFunnelHandler("", target, "").ServeHTTP(nw, r)
	if nw.Code != http.StatusNotFound {
		t.Errorf("with no name known yet: %d", nw.Code)
	}
}

// TestHelperFunnelNodeProcess is not a test. It is a child node that comes
// up, then reports its Funnel as the real one does: unavailable, then on.
func TestHelperFunnelNodeProcess(t *testing.T) {
	if os.Getenv("OUTPOST_HELPER_FUNNEL_NODE") != "1" {
		return
	}
	fmt.Println(HostLinePrefix + "tasking.tail.ts.net.")
	fmt.Println(ReadyLine)
	fmt.Println(FunnelLinePrefix + FunnelUnavailable + ` Funnel not available; "funnel" node attribute not set.`)
	if os.Getenv("OUTPOST_HELPER_FUNNEL_ON") == "1" {
		fmt.Println(FunnelLinePrefix + FunnelOn)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(0)
}

// A node's Funnel report reaches its status, and a refusal is said once in
// the log, with Tailscale's reason, rather than taking the tailnet side down.
func TestProcessNode_FunnelReachesStatus(t *testing.T) {
	for _, on := range []bool{false, true} {
		logs := &logSink{}
		r, err := New(Options{StateDir: t.TempDir(), Logf: logs.logf, Upstream: "http://127.0.0.1:1", PublicUpstream: "http://127.0.0.1:2"})
		if err != nil {
			t.Fatal(err)
		}
		var args []string
		r.nodeCmd = func(cfg nodeConfig) (*exec.Cmd, error) {
			args = nodeArgs(cfg)
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperFunnelNodeProcess$")
			cmd.Env = append(os.Environ(), "OUTPOST_HELPER_FUNNEL_NODE=1")
			if on {
				cmd.Env = append(cmd.Env, "OUTPOST_HELPER_FUNNEL_ON=1")
			}
			return cmd, nil
		}
		r.nodeStopGrace = 2 * time.Second

		svc := &manifest.Service{Name: "tasking", TailscaleName: "tasking", Port: 9310, Funnel: true}
		if err := r.Attach(context.Background(), svc); err != nil {
			t.Fatal(err)
		}
		want := NodeFunnelUnavailable
		if on {
			want = NodeFunnelOn
		}
		waitFor(t, 10*time.Second, "the funnel report", func() bool {
			st, _ := r.NodeStatus("tasking")
			return st.Funnel == want
		})
		st, _ := r.NodeStatus("tasking")
		if st.State != NodeOn {
			t.Errorf("on=%v: the tailnet side is %q", on, st.State)
		}
		if !on && !strings.Contains(st.FunnelError, `"funnel" node attribute not set`) {
			t.Errorf("reason = %q", st.FunnelError)
		}
		if !strings.Contains(strings.Join(args, " "), "--funnel --public-upstream http://127.0.0.1:2") {
			t.Errorf("args = %v", args)
		}
		if logs.has(FunnelLinePrefix) {
			t.Errorf("the protocol line was logged: %q", logs.snapshot())
		}
		if !logs.has("funnel unavailable") {
			t.Errorf("the refusal wasn't logged: %q", logs.snapshot())
		}
		_ = r.Close()
	}
}

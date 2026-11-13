package gateway

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// trafficHarness is the usual harness with wiki moved off yarr's port, since
// the meter can't tell two services on one port apart.
func trafficHarness(t *testing.T) (*harness, *http.Client) {
	t.Helper()
	h := newHarness(t)
	svcs := h.g.services.(*fakeServices)
	svcs.mu.Lock()
	for i := range svcs.list {
		if svcs.list[i].Name == "wiki" {
			svcs.list[i].Port = 1
		}
	}
	svcs.mu.Unlock()
	h.g.refreshRoutes(context.Background())
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))
	return h, c
}

func TestTraffic_GetCountsWhatTheAppSent(t *testing.T) {
	h, c := trafficHarness(t)
	before := h.g.Traffic()["yarr"]
	b := body(t, h.get(c, yarr+"/some/path"))
	after := h.g.Traffic()["yarr"]
	if got := after.Out - before.Out; got < uint64(len(b)) {
		t.Errorf("out grew by %d; want at least the %d-byte body", got, len(b))
	}
	if after.In <= before.In {
		t.Error("the request itself wasn't counted")
	}
}

func TestTraffic_PostCountsWhatWasSentToTheApp(t *testing.T) {
	h, c := trafficHarness(t)
	before := h.g.Traffic()["yarr"]
	payload := strings.Repeat("x", 20000)
	req, err := http.NewRequest(http.MethodPost, yarr+"/upload", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	backendSaw(t, resp)
	resp.Body.Close()
	after := h.g.Traffic()["yarr"]
	if got := after.In - before.In; got < uint64(len(payload)) {
		t.Errorf("in grew by %d; want at least the %d-byte body", got, len(payload))
	}
}

func TestTraffic_WebSocketIsCounted(t *testing.T) {
	h, c := trafficHarness(t)
	yarrURL, _ := url.Parse(yarr)
	var tok string
	for _, ck := range c.Jar.Cookies(yarrURL) {
		if ck.Name == "outpost" {
			tok = ck.Value
		}
	}
	conn, err := net.Dial("tcp", h.front.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second)) // a hung test fails on the read below either way
	if _, err := io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: yarr.outpost.test\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nCookie: outpost="+tok+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %v, %v", resp, err)
	}
	before := h.g.Traffic()["yarr"]
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatal(err)
	}
	// The bytes written to the backend are counted once that Write returns,
	// which can be just after the echo has come back on another goroutine, so
	// the count is waited for rather than read once.
	after := h.g.Traffic()["yarr"]
	for deadline := time.Now().Add(5 * time.Second); after.In-before.In < 5 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		after = h.g.Traffic()["yarr"]
	}
	if after.In-before.In < 5 || after.Out-before.Out < 5 {
		t.Errorf("websocket frames: in +%d out +%d; want at least 5 each way", after.In-before.In, after.Out-before.Out)
	}
}

func TestTraffic_OnlyRoutedPortsAreNamed(t *testing.T) {
	h, _ := trafficHarness(t)
	// A port no route has, and the port wiki was given.
	h.g.traffic.counter(9).in.Add(100)
	h.g.traffic.counter(1).out.Add(100)
	got := h.g.Traffic()
	for name := range got {
		if name != "yarr" && name != "wiki" {
			t.Errorf("traffic for %q, which isn't a route", name)
		}
	}
	if got["wiki"].Out != 100 {
		t.Errorf("wiki = %+v; want its port's count", got["wiki"])
	}

	// Two routes on one port: neither gets it.
	svcs := h.g.services.(*fakeServices)
	svcs.mu.Lock()
	svcs.list = append(svcs.list, supervisor.ServiceStatus{Name: "twin", Port: 1, Status: "running"})
	svcs.mu.Unlock()
	h.g.refreshRoutes(context.Background())
	got = h.g.Traffic()
	if _, ok := got["wiki"]; ok {
		t.Error("a shared port was put down to wiki")
	}
	if _, ok := got["twin"]; ok {
		t.Error("a shared port was put down to twin")
	}
}

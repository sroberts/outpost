package gateway

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/logging"
)

// lockedBuffer is a log shared by the gateway's handler goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// At debug, every request gets a line saying who it went through as or why
// it was refused, and none of the credentials it carried: not the password
// it signed in with, its session cookie, a good or a bad access token, nor a
// secret in its query.
func TestRequestLog_DebugLinesCarryNoSecrets(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.logf, h.g.logger = nil, logging.Component(logging.New(&logs, "debug", "logfmt"), "gateway", "debug")
	h.addUser(auth.User{Username: "alice"})

	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/feeds?code=planted-query-secret"))
	good := h.pat("alice")
	backendSaw(t, h.send(http.MethodGet, yarr+"/", bearer(good)))
	bad := auth.AccessTokenPrefix + "wrong-secret-value"
	if resp := h.send(http.MethodGet, yarr+"/", bearer(bad)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a bad token got %d", resp.StatusCode)
	}
	if resp := h.send(http.MethodGet, yarr+"/", nil); resp.StatusCode != http.StatusFound {
		t.Errorf("no credentials got %d; want the login redirect", resp.StatusCode)
	}

	out := logs.String()
	for _, want := range []string{"request_id=", "host=yarr.outpost.test", "path=/feeds", "status=200", "took=", "user=alice", `refused="not signed in"`, `refused="access token rejected"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the log lacks %q:\n%s", want, out)
		}
	}
	secrets := []string{pw, good, "wrong-secret", "planted-query-secret", "opat_"}
	for _, u := range []string{portal, yarr} {
		pu, _ := url.Parse(u)
		for _, ck := range c.Jar.Cookies(pu) {
			secrets = append(secrets, ck.Value)
		}
	}
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("%q reached the log:\n%s", s, out)
		}
	}
}

// At info, the gateway leaves requests out of the log, and doesn't wrap the
// response to time them.
func TestRequestLog_NothingAtInfo(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.logf, h.g.logger = nil, logging.Component(logging.New(&logs, "info", "logfmt"), "gateway", "info")
	h.send(http.MethodGet, yarr+"/", nil)
	if strings.Contains(logs.String(), "request_id=") {
		t.Errorf("a request was logged at info:\n%s", logs.String())
	}
}

// Logging a request mustn't cost it streaming or the upgrade to a WebSocket,
// which both reach past the response the line is timing.
func TestRequestLog_StreamsAndUpgradesStillWork(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.logf, h.g.logger = nil, logging.Component(logging.New(&logs, "debug", "logfmt"), "gateway", "debug")
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	backendSaw(t, h.login(c, "alice", "yarr", "/"))

	resp := h.get(c, yarr+"/events")
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Errorf("first line = %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event didn't arrive while the stream was open")
	}

	var tok string
	yarrURL, _ := url.Parse(yarr)
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
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second)) // the reads below fail if it doesn't hold
	if _, err := io.WriteString(conn, "GET /ws HTTP/1.1\r\nHost: yarr.outpost.test\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\nCookie: outpost="+tok+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	up, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if up.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("upgrade = %d", up.StatusCode)
	}
	if _, err := io.WriteString(conn, "hello"); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, 5)
	if _, err := io.ReadFull(br, echo); err != nil || string(echo) != "hello" {
		t.Errorf("echo = %q, %v", echo, err)
	}
}

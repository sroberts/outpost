package tsnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// echoService answers each line with "you said: <line>" until the client
// half-closes, then says "bye", so a test sees both directions and that a
// half-close is passed along rather than ending the connection.
func echoService(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					fmt.Fprintf(c, "you said: %s\n", sc.Text())
				}
				fmt.Fprintln(c, "bye")
			}()
		}
	}()
	return ln.Addr().String()
}

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, v...))
}

func (l *logLines) has(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

// serveForwarder runs f on a fresh loopback listener, standing in for the
// node's tailnet port, and returns its address.
func serveForwarder(t *testing.T, f *Forwarder) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return ln.Addr().String()
}

// talk sends a line, half-closes, and returns everything that comes back.
func talk(t *testing.T, addr string) string {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintln(c, "hello")
	_ = c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	return string(b)
}

func TestForwarder_UngatedCarriesBothWays(t *testing.T) {
	logs := &logLines{}
	addr := serveForwarder(t, &Forwarder{Service: "soft", Port: 22, Target: echoService(t), Logf: logs.logf})
	if got := talk(t, addr); got != "you said: hello\nbye\n" {
		t.Errorf("got %q; want the echo and, after the half-close, bye", got)
	}
	// The node's output is prefixed with the service already, so the
	// forwarder's own lines don't name it again.
	if !logs.has("tcp 22: connected") || logs.has("soft:") {
		t.Errorf("no connection logged: %v", logs.lines)
	}
}

func TestForwarder_GatedLetsAllowedCallersIn(t *testing.T) {
	logs := &logLines{}
	// Check runs on the forwarder's goroutine; a channel hands the
	// question back without racing the test.
	asked := make(chan string, 1)
	addr := serveForwarder(t, &Forwarder{
		Service: "soft", Port: 22, Target: echoService(t), Logf: logs.logf,
		Identify: func(context.Context, string) (Caller, error) {
			return Caller{Login: "alice@github", Device: "laptop"}, nil
		},
		Check: func(_ context.Context, service, login string) (string, error) {
			asked <- service + " " + login
			return "alice", nil
		},
	})
	if got := talk(t, addr); got != "you said: hello\nbye\n" {
		t.Errorf("got %q", got)
	}
	select {
	case q := <-asked:
		if q != "soft alice@github" {
			t.Errorf("asked the gateway about %q; want soft alice@github", q)
		}
	default:
		t.Error("the gateway was never asked")
	}
	if !logs.has("connected alice (alice@github)") {
		t.Errorf("log doesn't name the user: %v", logs.lines)
	}
}

// Every way a gated connection can fail to be admitted ends it before a
// byte reaches the service.
func TestForwarder_GatedRefusals(t *testing.T) {
	cases := map[string]struct {
		identify func(context.Context, string) (Caller, error)
		check    func(context.Context, string, string) (string, error)
		log      string
	}{
		"refused by the gateway": {
			identify: func(context.Context, string) (Caller, error) { return Caller{Login: "eve@github"}, nil },
			check: func(context.Context, string, string) (string, error) {
				return "", fmt.Errorf("%w: not an Outpost user's", ErrAccessRefused)
			},
			log: "not an Outpost user's",
		},
		"gateway unreachable": {
			identify: func(context.Context, string) (Caller, error) { return Caller{Login: "eve@github"}, nil },
			check: func(context.Context, string, string) (string, error) {
				return "", errors.New("connection refused")
			},
			log: "refusing rather than letting it through unchecked",
		},
		"tagged device": {
			identify: func(context.Context, string) (Caller, error) { return Caller{Tagged: true, Device: "ci-runner"}, nil },
			check: func(context.Context, string, string) (string, error) {
				return "", errors.New("must not be asked")
			},
			log: "ci-runner is tagged",
		},
		"unknown caller": {
			identify: func(context.Context, string) (Caller, error) { return Caller{}, errors.New("no such peer") },
			check: func(context.Context, string, string) (string, error) {
				return "", errors.New("must not be asked")
			},
			log: "can't tell who is calling",
		},
		"no way to identify": {
			check: func(context.Context, string, string) (string, error) { return "x", nil },
			log:   "can't tell who is calling",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var reached sync.WaitGroup
			svc, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer svc.Close()
			hit := make(chan struct{}, 1)
			reached.Add(1)
			go func() {
				defer reached.Done()
				if conn, err := svc.Accept(); err == nil {
					hit <- struct{}{}
					conn.Close()
				}
			}()

			logs := &logLines{}
			addr := serveForwarder(t, &Forwarder{
				Service: "soft", Port: 22, Target: svc.Addr().String(), Logf: logs.logf,
				Identify: c.identify, Check: c.check,
			})
			if got := talk(t, addr); got != "" {
				t.Errorf("refused caller got %q back", got)
			}
			svc.Close()
			reached.Wait()
			select {
			case <-hit:
				t.Error("the service was dialled for a refused caller")
			default:
			}
			if !logs.has("refused") || !logs.has(c.log) {
				t.Errorf("log %v; want a refusal saying %q", logs.lines, c.log)
			}
		})
	}
}

func TestForwarder_ServiceDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := ln.Addr().String()
	ln.Close()
	logs := &logLines{}
	addr := serveForwarder(t, &Forwarder{Service: "soft", Port: 22, Target: dead, Logf: logs.logf})
	if got := talk(t, addr); got != "" {
		t.Errorf("got %q from a service that isn't there", got)
	}
	if !logs.has("is not reachable") {
		t.Errorf("log %v; want it to say the service is down", logs.lines)
	}
}

func TestAccessChecker_Answers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AccessPath || r.Header.Get(AccessSecretHeader) != "k" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		switch r.FormValue("login") {
		case "ok@x":
			fmt.Fprint(w, `{"user":"ok"}`)
		case "no@x":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"reason":"nope"}`)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c, err := NewAccessChecker(srv.URL, "", "k")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if u, err := c.Check(ctx, "soft", "ok@x"); err != nil || u != "ok" {
		t.Errorf("allowed: %q, %v", u, err)
	}
	if _, err := c.Check(ctx, "soft", "no@x"); !errors.Is(err, ErrAccessRefused) || !strings.Contains(err.Error(), "nope") {
		t.Errorf("refused: %v", err)
	}
	if _, err := c.Check(ctx, "soft", "err@x"); err == nil || errors.Is(err, ErrAccessRefused) {
		t.Errorf("server error: %v; want a failure that isn't a refusal", err)
	}
	if _, err := NewAccessChecker(srv.URL, "", ""); err == nil {
		t.Error("a checker with no secret was built")
	}
}

func TestNodeArgs_CarryForwards(t *testing.T) {
	args := strings.Join(nodeArgs(nodeConfig{
		Service: "soft", Hostname: "soft", Dir: "/d", Port: 23232,
		TCP: []manifest.TCPForward{{Tailnet: 22, Local: 23231}, {Tailnet: 9418, Local: 9418}},
	}), " ")
	if !strings.Contains(args, "--tcp 22=23231 --tcp 9418=9418") {
		t.Errorf("args %q; want both forwards", args)
	}
	// The secret goes by environment, never on the command line.
	if strings.Contains(strings.Join(nodeArgs(nodeConfig{AccessSecret: "hidden"}), " "), "hidden") {
		t.Error("the access secret is in the node's arguments")
	}
}

// Changing a service's forwards replaces its node, as a changed port does;
// an unchanged set leaves it alone.
func TestAttach_ChangedForwardsReplaceTheNode(t *testing.T) {
	var mu sync.Mutex
	var started []nodeConfig
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		mu.Lock()
		started = append(started, cfg)
		mu.Unlock()
		return newFakeNode(t), nil
	})
	r.upstream = "http://127.0.0.1:1"
	r.accessSecret = "k"
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(started) }

	s := svc("soft", 23232)
	s.TCP = map[string]int{"22": 23231}
	if err := r.Attach(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "the first node", func() bool { return count() == 1 })
	if err := r.Attach(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	s2 := svc("soft", 23232)
	s2.TCP = map[string]int{"22": 23231, "9418": 9418}
	if err := r.Attach(context.Background(), s2); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "a replacement node", func() bool { return count() == 2 })

	mu.Lock()
	defer mu.Unlock()
	if len(started[1].TCP) != 2 || started[1].AccessSecret != "k" {
		t.Errorf("replacement got %+v; want both forwards and the secret", started[1])
	}
}

// A node with nothing to gate isn't handed the secret.
func TestAttach_SecretOnlyForGatedForwards(t *testing.T) {
	got := make(chan nodeConfig, 1)
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		got <- cfg
		return newFakeNode(t), nil
	})
	r.upstream = "http://127.0.0.1:1"
	r.accessSecret = "k"
	if err := r.Attach(context.Background(), svc("plain", 9300)); err != nil {
		t.Fatal(err)
	}
	if cfg := <-got; cfg.AccessSecret != "" {
		t.Error("a node with no forwards was given the access secret")
	}
}

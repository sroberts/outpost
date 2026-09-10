package tsnet

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// backend starts a local server standing in for a supervised service.
func backend(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

func TestProxy_ForwardsRequestAndResponse(t *testing.T) {
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Service", "yes")
		fmt.Fprintf(w, "path=%s method=%s", r.URL.Path, r.Method)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://demo.tailnet.ts.net/v1/things", nil)
	newProxy(port).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "path=/v1/things") {
		t.Errorf("path not forwarded: %q", body)
	}
	if rec.Header().Get("X-Service") != "yes" {
		t.Error("response headers not forwarded")
	}
}

// The service should see the tailnet name the caller used, so any absolute
// URLs it builds are reachable.
func TestProxy_PreservesHostHeader(t *testing.T) {
	var seenHost string
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		seenHost = r.Host
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "http://plumbline.tailnet.ts.net/up", nil)
	newProxy(port).ServeHTTP(httptest.NewRecorder(), req)

	if seenHost != "plumbline.tailnet.ts.net" {
		t.Errorf("backend saw Host %q; want the tailnet name", seenHost)
	}
}

func TestProxy_ForwardsRequestBody(t *testing.T) {
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "got:%s", body)
	})

	req := httptest.NewRequest(http.MethodPost, "http://demo/v1/write", strings.NewReader("payload"))
	rec := httptest.NewRecorder()
	newProxy(port).ServeHTTP(rec, req)

	if got := rec.Body.String(); got != "got:payload" {
		t.Errorf("body = %q; want %q", got, "got:payload")
	}
}

func TestProxy_PropagatesServiceStatusCodes(t *testing.T) {
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	})

	rec := httptest.NewRecorder()
	newProxy(port).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://demo/up", nil))
	if rec.Code != http.StatusTeapot {
		t.Errorf("status = %d; want %d", rec.Code, http.StatusTeapot)
	}
}

// A service that is down or restarting must produce a clear 502, not a panic
// or a bare 500.
func TestProxy_UnreachableServiceGives502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing is listening now

	rec := httptest.NewRecorder()
	newProxy(port).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://demo/up", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "not reachable") {
		t.Errorf("unhelpful body: %q", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), strconv.Itoa(port)) {
		t.Errorf("body does not name the unreachable address: %q", rec.Body.String())
	}
}

func TestProxy_SetsForwardedHeaders(t *testing.T) {
	var forwardedFor string
	port := backend(t, func(w http.ResponseWriter, r *http.Request) {
		forwardedFor = r.Header.Get("X-Forwarded-For")
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "http://demo/up", nil)
	req.RemoteAddr = "100.64.0.7:12345" // a tailnet address
	newProxy(port).ServeHTTP(httptest.NewRecorder(), req)

	if forwardedFor != "100.64.0.7" {
		t.Errorf("X-Forwarded-For = %q; want the tailnet client address", forwardedFor)
	}
}

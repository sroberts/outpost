package tsnet

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// A node in front of the gateway leaves the wait for headers to the gateway.
// An MCP tool call that takes minutes before answering used to come back as a
// 502 from the node at the one-minute mark, whatever the gateway allowed.
func TestUpstreamProxy_WaitsPastTheServiceHeaderTimeout(t *testing.T) {
	old := serviceHeaderTimeout
	serviceHeaderTimeout = 100 * time.Millisecond
	t.Cleanup(func() { serviceHeaderTimeout = old })

	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}

	// Straight to a service, the limit still applies...
	port := backend(t, slow)
	rec := httptest.NewRecorder()
	newProxy(port).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://demo/slow", nil))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("direct proxy: status = %d; want 502 once the shortened timeout passes", rec.Code)
	}

	// ...but in front of the gateway it does not.
	gw := httptest.NewServer(http.HandlerFunc(slow))
	t.Cleanup(gw.Close)
	target, _ := url.Parse(gw.URL)
	rec = httptest.NewRecorder()
	NewUpstreamProxy(target, "").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://demo/slow", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("upstream proxy: status = %d; want 200 after waiting past the service timeout", rec.Code)
	}
}

func TestUpstreamProxy_HasNoHeaderTimeout(t *testing.T) {
	target, _ := url.Parse("http://127.0.0.1:8443")
	tr, ok := NewUpstreamProxy(target, "").Transport.(*http.Transport)
	if !ok {
		t.Fatal("the upstream proxy's transport is not an *http.Transport")
	}
	if tr.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %s; want none", tr.ResponseHeaderTimeout)
	}
}

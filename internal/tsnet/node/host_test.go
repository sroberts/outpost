package node

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A peer allowed to reach only this node must not reach the portal or
// another app through it by naming them in Host.
func TestOnlyHost_RefusesOtherNames(t *testing.T) {
	passed := false
	h := onlyHost("yarr-1.example.ts.net.", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { passed = true }))

	for _, host := range []string{
		"yarr-1.example.ts.net",
		"YARR-1.Example.ts.net.",
		"yarr-1.example.ts.net:443",
	} {
		passed = false
		req := httptest.NewRequest(http.MethodGet, "/feeds", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if !passed || rec.Code != http.StatusOK {
			t.Errorf("Host %q = %d, passed %v; want it served", host, rec.Code, passed)
		}
	}

	for _, host := range []string{
		"outpost.example.ts.net",
		"wiki-1.example.ts.net",
		"yarr-1.example.ts.net.evil",
		"100.64.0.1",
		"",
	} {
		passed = false
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if passed || rec.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q = %d, passed %v; want 421 and not passed on", host, rec.Code, passed)
		}
	}
}

// Before the node knows its name there is nothing to match, so everything
// is refused rather than let through.
func TestOnlyHost_RefusesEverythingWithoutAName(t *testing.T) {
	h := onlyHost("", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("request passed on before the node had a name")
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "yarr-1.example.ts.net"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMisdirectedRequest {
		t.Errorf("status = %d; want 421", rec.Code)
	}
}

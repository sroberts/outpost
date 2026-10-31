package gateway

import (
	"net/http/httptest"
	"testing"
)

// Dot segments that path.Clean leaves alone but an app may still resolve,
// via servlet ";params" or a second round of decoding, must not be public.
func TestIsPublic_RefusesDotSegmentsAppsResolve(t *testing.T) {
	g := &Gateway{}
	rt := route{PublicPaths: []string{"/hooks/*", "/.well-known/*", "/feed.xml"}}
	for _, tc := range []struct {
		target string
		public bool
	}{
		{"/hooks/a", true},
		{"/.well-known/x", true},
		{"/feed.xml", true},
		{"/hooks/a;v=1", true},
		{"/hooks/..;/admin", false},
		{"/hooks/%2e%2e;/admin", false},
		{"/hooks/%252e%252e/admin", false},
		{"/hooks/.%2e/admin", false},
		{"/hooks/.;/admin", false},
		{"/hooks/..x", false},
	} {
		r := httptest.NewRequest("GET", "http://yarr.outpost.test"+tc.target, nil)
		if got := g.isPublic(r, rt); got != tc.public {
			t.Errorf("isPublic(%q) = %v; want %v", tc.target, got, tc.public)
		}
	}
}

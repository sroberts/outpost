package gateway

import "testing"

func TestClientLabel(t *testing.T) {
	for _, tc := range []struct{ name, id, want string }{
		{"Claude", "https://claude.ai/oauth/claude-client-metadata", "Claude (claude.ai)"},
		{"", "https://claude.ai/oauth/x", "claude.ai"},
		{"claude.ai", "https://claude.ai/oauth/x", "claude.ai"},
		{"Claude", "not a url", "Claude (not a url)"},
	} {
		if got := ClientLabel(tc.name, tc.id); got != tc.want {
			t.Errorf("ClientLabel(%q, %q) = %q; want %q", tc.name, tc.id, got, tc.want)
		}
	}
}

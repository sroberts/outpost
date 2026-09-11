package tsnet

import "testing"

// The parser matches on shape, so it has to accept the line tsnet really
// prints and keep accepting rewordings of it, while refusing a line that
// merely happens to carry a URL.
func TestParseLoginURL(t *testing.T) {
	cases := []struct {
		name string
		line string
		want string
	}{
		{
			// Verbatim from tsnet v1.102.3's printAuthURLLoop.
			name: "the line tsnet actually prints",
			line: "To start this tsnet server, restart with TS_AUTHKEY set, or go to: https://login.tailscale.com/a/0f1e2d3c4b5a",
			want: "https://login.tailscale.com/a/0f1e2d3c4b5a",
		},
		{
			name: "a reworded prompt",
			line: "To authenticate, visit https://login.tailscale.com/a/abcdef",
			want: "https://login.tailscale.com/a/abcdef",
		},
		{
			name: "log in, spelled as two words",
			line: "please log in at https://login.tailscale.com/a/abcdef",
			want: "https://login.tailscale.com/a/abcdef",
		},
		{
			name: "trailing punctuation is not part of the URL",
			line: "To authenticate, visit https://login.tailscale.com/a/abcdef.",
			want: "https://login.tailscale.com/a/abcdef",
		},
		{
			// The hint has to come from the prose: every login URL contains
			// the word "login" whatever the line around it says.
			name: "a URL in a line that is not an auth prompt",
			line: "health(warning): control server https://login.tailscale.com is unreachable",
			want: "",
		},
		{
			name: "a prompt with no URL yet",
			line: "authenticating; no URL from control yet",
			want: "",
		},
		{
			name: "a bare scheme is not a URL",
			line: "to log in, go to: https://",
			want: "",
		},
		{
			name: "ordinary chatter",
			line: "magicsock: home is derp-10 (sea)",
			want: "",
		},
		{
			name: "nothing at all",
			line: "",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseLoginURL(tc.line)
			if ok != (tc.want != "") {
				t.Fatalf("parseLoginURL(%q) ok = %v; want %v", tc.line, ok, tc.want != "")
			}
			if got != tc.want {
				t.Errorf("parseLoginURL(%q) = %q; want %q", tc.line, got, tc.want)
			}
		})
	}
}

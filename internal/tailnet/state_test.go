package tailnet

import "testing"

// The invariant every existing renderer test depends on: nothing reports
// state yet, so StateUnknown must reproduce exactly what today's code prints,
// for both a configured name and an unconfigured one.
func TestColumnAndLine_StateUnknownReproducesTodaysOutput(t *testing.T) {
	for _, name := range []string{"plumbline", ""} {
		n := NodeStatus{}

		wantColumn := orDash(name)
		if got := Column(n, name); got != wantColumn {
			t.Errorf("Column(StateUnknown, %q) = %q; want %q", name, got, wantColumn)
		}
		if got := Line(n, name); got != name {
			t.Errorf("Line(StateUnknown, %q) = %q; want %q", name, got, name)
		}
	}
}

func TestColumn(t *testing.T) {
	for _, tc := range []struct {
		name           string
		n              NodeStatus
		configuredName string
		want           string
	}{
		{"disabled", NodeStatus{State: StateDisabled}, "plumbline", "-"},
		{"offline", NodeStatus{State: StateOffline}, "plumbline", "-"},
		{"starting", NodeStatus{State: StateStarting}, "plumbline", "connecting"},
		{"needs login", NodeStatus{State: StateNeedsLogin}, "plumbline", "needs login"},
		{"failed", NodeStatus{State: StateFailed, Detail: "timeout"}, "plumbline", "failed"},
		{"attached with hostname", NodeStatus{State: StateAttached, Hostname: "plumbline-abc"}, "plumbline", "plumbline-abc"},
		{"attached, hostname empty falls back to configured", NodeStatus{State: StateAttached}, "plumbline", "plumbline"},
		{"attached, nothing at all", NodeStatus{State: StateAttached}, "", "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Column(tc.n, tc.configuredName); got != tc.want {
				t.Errorf("Column() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestLine(t *testing.T) {
	for _, tc := range []struct {
		name           string
		n              NodeStatus
		configuredName string
		want           string
	}{
		{"disabled", NodeStatus{State: StateDisabled}, "plumbline", "-"},
		{"offline", NodeStatus{State: StateOffline}, "plumbline", "plumbline (no node; the service is not running)"},
		{"starting", NodeStatus{State: StateStarting}, "plumbline", "plumbline — connecting"},
		{"needs login", NodeStatus{State: StateNeedsLogin}, "plumbline", "plumbline — needs login"},
		{"failed with detail", NodeStatus{State: StateFailed, Detail: "timeout"}, "plumbline", "plumbline — failed: timeout"},
		{"failed without detail", NodeStatus{State: StateFailed}, "plumbline", "plumbline — failed"},
		{"attached with addr", NodeStatus{State: StateAttached, Addr: "100.64.1.2"}, "plumbline", "plumbline (attached, 100.64.1.2)"},
		{"attached without addr", NodeStatus{State: StateAttached}, "plumbline", "plumbline (attached)"},
		{"attached with hostname override", NodeStatus{State: StateAttached, Hostname: "plumbline-abc", Addr: "100.64.1.2"}, "plumbline", "plumbline-abc (attached, 100.64.1.2)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Line(tc.n, tc.configuredName); got != tc.want {
				t.Errorf("Line() = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestLabel_CoversEveryStateExceptUnknown(t *testing.T) {
	for _, tc := range []struct {
		s    State
		want string
	}{
		{StateDisabled, "disabled"},
		{StateOffline, "offline"},
		{StateStarting, "connecting"},
		{StateNeedsLogin, "needs login"},
		{StateAttached, "attached"},
		{StateFailed, "failed"},
		{StateUnknown, ""},
	} {
		if got := tc.s.Label(); got != tc.want {
			t.Errorf("State(%q).Label() = %q; want %q", tc.s, got, tc.want)
		}
	}
}

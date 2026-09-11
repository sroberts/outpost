package tailnet

import "testing"

// The column is narrow and the suffix is the same on every row, so the normal
// case shows the short name. A node that registered under a genuinely
// different name is a discrepancy, and hiding that would repeat the mistake
// this package exists to undo: reporting the configured name as though it were
// the observed one.
func TestColumn_ShortNameNormallyButTheRegisteredNameWhenItDiffers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hostname   string
		configured string
		want       string
	}{
		{"fqdn of the configured name shows short", "yarr.tailcad888.ts.net", "yarr", "yarr"},
		{"bare match shows short", "yarr", "yarr", "yarr"},
		{"a different registered name is shown in full", "yarr-2.tailcad888.ts.net", "yarr", "yarr-2.tailcad888.ts.net"},
		{"a suffixed name is a discrepancy", "plumbline-abc", "plumbline", "plumbline-abc"},
		{"no hostname falls back to configured", "", "yarr", "yarr"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Column(NodeStatus{State: StateAttached, Hostname: tc.hostname}, tc.configured)
			if got != tc.want {
				t.Errorf("Column() = %q; want %q", got, tc.want)
			}
		})
	}
}

// status has room, so it always carries the full registered name and the
// address — the other half of "both are critical".
func TestLine_AttachedCarriesTheFullNameAndAddress(t *testing.T) {
	got := Line(NodeStatus{
		State: StateAttached, Hostname: "yarr.tailcad888.ts.net", Addr: "100.73.39.90",
	}, "yarr")
	if want := "yarr.tailcad888.ts.net (attached, 100.73.39.90)"; got != want {
		t.Errorf("Line() = %q; want %q", got, want)
	}
}

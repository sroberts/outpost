package supervisor

import (
	"strings"
	"testing"
)

// reportingTailnet is a fakeTailnet that can also say how a node is doing.
type reportingTailnet struct {
	*fakeTailnet
	nodes map[string]TailnetStatus
}

func (r *reportingTailnet) TailnetStatus(name string) (TailnetStatus, bool) {
	st, ok := r.nodes[name]
	return st, ok
}

// Status carries what the router observed, not only the manifest's name.
func TestStatus_ReportsTheNodeNotJustTheName(t *testing.T) {
	h := newHarness(t)
	rt := &reportingTailnet{fakeTailnet: newFakeTailnet(), nodes: map[string]TailnetStatus{
		"alpha": {State: TailnetNeedsLogin, LoginURL: "https://login.example/a/1"},
	}}
	h.sup.tailnet = rt
	h.writeManifest("alpha", "")
	h.writeManifest("beta", "")
	h.apply()
	h.tick()

	if st := h.statusOf("alpha"); st.Tailnet == nil || *st.Tailnet != rt.nodes["alpha"] {
		t.Errorf("alpha's tailnet = %+v", st.Tailnet)
	}
	if st := h.statusOf("beta"); st.Tailnet == nil || st.Tailnet.State != TailnetNone {
		t.Errorf("beta, with no node, = %+v; want none", st.Tailnet)
	}
}

// With Tailscale off there is no router, and the status says so rather than
// showing a name nothing answers to.
func TestStatus_TailnetOffWithoutARouter(t *testing.T) {
	h := newHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	if st := h.statusOf("alpha"); st.Tailnet == nil || st.Tailnet.State != TailnetOff {
		t.Errorf("tailnet = %+v; want off", st.Tailnet)
	}
}

// A router that can't report leaves the field out, as an old daemon does.
func TestStatus_RouterThatCannotReport(t *testing.T) {
	h, _ := tailnetHarness(t)
	h.writeManifest("alpha", "")
	h.apply()
	if st := h.statusOf("alpha"); st.Tailnet != nil {
		t.Errorf("tailnet = %+v; want nil", st.Tailnet)
	}
}

func TestTailnetSummaryAndDetail(t *testing.T) {
	base := ServiceStatus{TailscaleName: "yarr"}
	for _, tc := range []struct {
		tailnet         *TailnetStatus
		summary, detail string
	}{
		{nil, "yarr", "yarr"},
		{&TailnetStatus{State: TailnetOn, Host: "yarr-1.tail1234.ts.net"}, "yarr-1", "yarr-1.tail1234.ts.net"},
		{&TailnetStatus{State: TailnetOn}, "yarr", "yarr"},
		{&TailnetStatus{State: TailnetNeedsLogin, LoginURL: "u"}, "needs login", "yarr (needs login)"},
		{&TailnetStatus{State: TailnetStarting}, "starting", "yarr (starting)"},
		{&TailnetStatus{State: TailnetStarting, Error: "boom"}, "retrying", "yarr (starting; last attempt: boom)"},
		{&TailnetStatus{State: TailnetNone}, "", "yarr (not on the tailnet)"},
		{&TailnetStatus{State: TailnetOff}, "", "Tailscale is off"},
	} {
		s := base
		s.Tailnet = tc.tailnet
		if got := s.TailnetSummary(); got != tc.summary {
			t.Errorf("%+v: summary = %q; want %q", tc.tailnet, got, tc.summary)
		}
		if got := s.TailnetDetail(); !strings.Contains(got, tc.detail) {
			t.Errorf("%+v: detail = %q; want it to contain %q", tc.tailnet, got, tc.detail)
		}
	}
}

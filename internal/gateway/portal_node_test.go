package gateway

import "testing"

func TestStatus_ReportsThePortalsNode(t *testing.T) {
	h := newHarness(t)
	if n := h.g.Status().Node; n != nil {
		t.Fatalf("a portal without a node reported one: %+v", n)
	}

	report := PortalNode{State: "needs-login", LoginURL: "https://login.tailscale.com/a/x"}
	h.g.nodeStatus = func() (PortalNode, bool) { return report, true }
	n := h.g.Status().Node
	if n == nil || n.State != "needs-login" || n.LoginURL != report.LoginURL || n.Renamed {
		t.Fatalf("Node = %+v; want the login it is waiting on", n)
	}

	// The portal is at http://outpost.test; a node by that name is where it should be.
	report = PortalNode{State: "on", Host: "Outpost.Test."}
	if n := h.g.Status().Node; n == nil || n.Renamed {
		t.Errorf("Node = %+v; want it on, under public_url's host", n)
	}

	// Tailscale gave it another name: public_url now names nothing.
	report = PortalNode{State: "on", Host: "outpost-1.test"}
	if n := h.g.Status().Node; n == nil || !n.Renamed {
		t.Errorf("Node = %+v; want it flagged as renamed", n)
	}

	h.g.nodeStatus = func() (PortalNode, bool) { return PortalNode{}, false }
	if n := h.g.Status().Node; n != nil {
		t.Errorf("Node = %+v; want none while the router has none", n)
	}
}

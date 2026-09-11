package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
	"github.com/sroberts/outpost/internal/tailnet"
)

// The `outpost list` TAILSCALE cell must stay byte-identical to today's
// output when nothing has reported tailnet state — that is what every list
// test predating this feature exercises.
func TestWriteTable_StateUnknownIsUnchanged(t *testing.T) {
	for _, name := range []string{"plumbline", ""} {
		var buf bytes.Buffer
		s := supervisor.ServiceStatus{Name: "plumbline", TailscaleName: name}
		if err := writeTable(&buf, []supervisor.ServiceStatus{s}); err != nil {
			t.Fatal(err)
		}
		got := buf.String()
		if !strings.Contains(got, orDash(name)) {
			t.Errorf("TailscaleName %q: TAILSCALE column missing %q:\n%s", name, orDash(name), got)
		}
	}
}

func TestWriteTable_TailscaleColumnReflectsState(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    tailnet.NodeStatus
		want string
	}{
		{"disabled", tailnet.NodeStatus{State: tailnet.StateDisabled}, "-"},
		{"offline", tailnet.NodeStatus{State: tailnet.StateOffline}, "-"},
		{"needs login", tailnet.NodeStatus{State: tailnet.StateNeedsLogin}, "needs login"},
		{"attached", tailnet.NodeStatus{State: tailnet.StateAttached, Hostname: "plumbline"}, "plumbline"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := supervisor.ServiceStatus{Name: "plumbline", TailscaleName: "plumbline", Tailnet: tc.n}
			if err := writeTable(&buf, []supervisor.ServiceStatus{s}); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("TAILSCALE column missing %q:\n%s", tc.want, buf.String())
			}
		})
	}
}

// `outpost status` gains the login URL and its hint only in StateNeedsLogin —
// every other state must show exactly the tailnet.Line() value and nothing else.
func TestWriteTailnetDetail_LoginLinesOnlyInNeedsLogin(t *testing.T) {
	for _, tc := range []struct {
		name       string
		n          tailnet.NodeStatus
		wantURL    bool
		wantURLstr string
	}{
		{"unknown", tailnet.NodeStatus{}, false, ""},
		{"disabled", tailnet.NodeStatus{State: tailnet.StateDisabled}, false, ""},
		{"attached", tailnet.NodeStatus{State: tailnet.StateAttached, Hostname: "plumbline"}, false, ""},
		{"needs login", tailnet.NodeStatus{State: tailnet.StateNeedsLogin, LoginURL: "https://login.tailscale.com/a/abc"}, true, "https://login.tailscale.com/a/abc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			st := supervisor.ServiceStatus{Name: "plumbline", TailscaleName: "plumbline", Tailnet: tc.n}
			writeTailnetDetail(&buf, st)
			got := buf.String()

			if !strings.Contains(got, tailnet.Line(tc.n, st.TailscaleName)) {
				t.Errorf("tailscale line missing tailnet.Line() output:\n%s", got)
			}
			if tc.wantURL {
				if !strings.Contains(got, tc.wantURLstr) {
					t.Errorf("needs-login block missing the login URL:\n%s", got)
				}
				if !strings.Contains(got, "authorise the node") || !strings.Contains(got, "auth key") {
					t.Errorf("needs-login block missing the recourse hint:\n%s", got)
				}
			} else if strings.Contains(got, "http") {
				t.Errorf("%s: login URL emitted outside StateNeedsLogin:\n%s", tc.name, got)
			}
		})
	}
}

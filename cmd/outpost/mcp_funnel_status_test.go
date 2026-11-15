package main

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

func TestMCPFunnelLine(t *testing.T) {
	st := supervisor.ServiceStatus{Name: "sous", MCPPath: "/mcp", MCPFunnel: true}
	for _, tc := range []struct {
		tailnet *supervisor.TailnetStatus
		want    []string
	}{
		{nil, []string{"once its node is up"}},
		{&supervisor.TailnetStatus{Funnel: supervisor.FunnelStarting}, []string{"starting"}},
		{&supervisor.TailnetStatus{Funnel: supervisor.FunnelOn, Host: "sous.tail.ts.net"},
			[]string{"on: https://sous.tail.ts.net/mcp", "internet access tokens only", "--internet --app sous"}},
		{&supervisor.TailnetStatus{Funnel: supervisor.FunnelUnavailable, FunnelError: "no funnel attribute"},
			[]string{"unavailable: no funnel attribute"}},
	} {
		st.Tailnet = tc.tailnet
		got := mcpFunnelLine(st)
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: %q lacks %q", tc.tailnet, got, w)
			}
		}
	}
}

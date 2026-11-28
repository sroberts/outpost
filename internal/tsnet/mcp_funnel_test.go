package tsnet

import (
	"context"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// An app with only mcp_funnel gets a Funnel listener on its node, as one with
// funnel does; with neither it gets none.
func TestAttach_MCPFunnelStartsTheFunnelListener(t *testing.T) {
	r, err := New(Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	svc := &manifest.Service{Name: "sous", TailscaleName: "sous", Port: 1234, MCPPath: "/mcp", MCPFunnel: true}
	if err := r.Attach(context.Background(), svc); err == nil {
		t.Fatal("Attach with mcp_funnel and no gateway: want error, got nil")
	}

	for _, on := range []bool{true, false} {
		gated, err := New(Options{StateDir: t.TempDir(), Upstream: "http://127.0.0.1:1", PublicUpstream: "http://127.0.0.1:2"})
		if err != nil {
			t.Fatal(err)
		}
		started := make(chan nodeConfig, 1)
		gated.startNode = func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
			select {
			case started <- cfg:
			default:
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		svc.MCPFunnel = on
		if err := gated.Attach(context.Background(), svc); err != nil {
			t.Fatalf("mcp_funnel %v: %v", on, err)
		}
		select {
		case cfg := <-started:
			if cfg.Funnel != on || (on && cfg.PublicUpstream != "http://127.0.0.1:2") {
				t.Errorf("mcp_funnel %v: node config funnel %v, public upstream %q", on, cfg.Funnel, cfg.PublicUpstream)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("mcp_funnel %v: the node was never started", on)
		}
		_ = gated.Close()
	}
}

func TestFunnelFor(t *testing.T) {
	for _, tc := range []struct {
		funnel, mcp bool
		want        string
	}{
		{true, false, "its public_paths"},
		{false, true, "its MCP endpoint /mcp, for internet access tokens"},
		{true, true, "its public_paths and its MCP endpoint /mcp, for internet access tokens"},
	} {
		svc := &manifest.Service{Funnel: tc.funnel, MCPFunnel: tc.mcp, MCPPath: "/mcp"}
		if got := funnelFor(svc); got != tc.want {
			t.Errorf("funnel %v, mcp_funnel %v: %q; want %q", tc.funnel, tc.mcp, got, tc.want)
		}
	}
}

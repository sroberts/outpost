package ipc

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/gateway"
)

type fakeGateway struct{ st gateway.Status }

func (f fakeGateway) Status() gateway.Status { return f.st }

func serveWith(t *testing.T, configure func(*Server)) *Client {
	t.Helper()
	sup := newIdleSupervisor(t, t.TempDir())
	sock := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sock, sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	configure(srv)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sock)
}

func TestGatewayStatus(t *testing.T) {
	want := gateway.Status{
		Listen:    "127.0.0.1:8443",
		PublicURL: "https://outpost.example",
		Domain:    "outpost.example",
		Routes:    []gateway.RouteStatus{{Name: "yarr", URL: "https://yarr.outpost.example", Port: 7070, Status: "running"}},
	}
	c := serveWith(t, func(s *Server) { s.UseGateway(fakeGateway{want}) })
	got, err := c.GatewayStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got.PublicURL != want.PublicURL || len(got.Routes) != 1 || got.Routes[0] != want.Routes[0] {
		t.Errorf("status = %+v", got)
	}
}

func TestGatewayStatus_Off(t *testing.T) {
	c := serveWith(t, func(*Server) {})
	if _, err := c.GatewayStatus(); err == nil || !strings.Contains(err.Error(), "gateway is off") {
		t.Errorf("err = %v; want an explanation that the gateway is off", err)
	}
}

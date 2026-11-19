package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/supervisor"
)

type listOnly []supervisor.ServiceStatus

func (l listOnly) List(context.Context) ([]supervisor.ServiceStatus, error) { return l, nil }

// The gateway's source reads one service, or every declared one, and a
// declared service with no log yet is an empty log rather than an error.
func TestLogsSource(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, "web.log"), applog.Format(at, applog.Stdout, "hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "undeclared.log"), applog.Format(at, applog.Stdout, "stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := logsSource{dir: dir, services: listOnly{{Name: "web"}, {Name: "quiet"}}}

	recs, err := src.Logs(context.Background(), "quiet", applog.Query{})
	if err != nil || recs == nil || len(recs) != 0 {
		t.Errorf("a silent service: %v, %v", recs, err)
	}
	recs, err = src.Logs(context.Background(), "", applog.Query{})
	if err != nil || len(recs) != 1 || recs[0].Service != "web" || recs[0].Message != "hello" {
		t.Errorf("every service: %+v, %v", recs, err)
	}
	if _, err := src.Logs(context.Background(), "../etc", applog.Query{}); err == nil {
		t.Error("a name outside the directory was read")
	}
}

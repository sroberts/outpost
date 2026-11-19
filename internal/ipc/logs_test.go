package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
)

func logsDaemon(t *testing.T, dir string) *Client {
	t.Helper()
	sup := newIdleSupervisor(t, t.TempDir())
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseLogs(dir)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sockPath)
}

func TestLogs_ReadsFiltersAndRefusesPaths(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	body := string(applog.Format(at, applog.Stdout, "hello")) +
		string(applog.Format(at.Add(time.Second), applog.Stderr, `{"level":"error","msg":"db locked","table":"users"}`))
	if err := os.WriteFile(filepath.Join(dir, "web.log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := logsDaemon(t, dir)

	all, err := c.Logs("web", LogsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Message != "hello" || all[1].Level != applog.LevelError ||
		all[1].Fields["table"] != "users" || all[1].Stream != applog.Stderr || !all[1].Time.Equal(at.Add(time.Second)) {
		t.Fatalf("logs = %+v", all)
	}
	errs, err := c.Logs("web", LogsRequest{Level: "error"})
	if err != nil || len(errs) != 1 || errs[0].Message != "db locked" {
		t.Errorf("level=error: %+v, %v", errs, err)
	}

	// A service that hasn't written anything yet reads as empty.
	if got, err := c.Logs("quiet", LogsRequest{}); err != nil || len(got) != 0 {
		t.Errorf("no log yet: %v, %v", got, err)
	}
	// A name that would read outside the logs directory is refused.
	if _, err := c.Logs("../secret", LogsRequest{}); err == nil || !strings.Contains(err.Error(), "no such service") {
		t.Errorf("path in a name: %v", err)
	}
}

func TestLogs_RepliesStayBounded(t *testing.T) {
	var recs []applog.Record
	for range 100 {
		recs = append(recs, applog.Record{Line: strings.Repeat("x", 1000), Message: "m"})
	}
	recs[99].Message = "newest"
	got := capLogBytes(recs, 10_500)
	if len(got) != 10 || got[len(got)-1].Message != "newest" {
		t.Errorf("kept %d records, last %q; want the newest 10", len(got), got[len(got)-1].Message)
	}
}

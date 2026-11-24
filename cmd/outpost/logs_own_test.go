package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/supervisor"
)

// writeOwnLogs plants a daemon log and an access log in home, as the daemon
// would write them.
func writeOwnLogs(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	daemon := at.Format("2006-01-02 15:04:05") + " INFO supervising 1 service(s)\n" +
		at.Add(time.Second).Format("2006-01-02 15:04:05") + " WARN supervisor: alpha: unhealthy service=alpha\n" +
		at.Add(2*time.Second).Format("2006-01-02 15:04:05") + " INFO tsnet: alpha: on the tailnet service=alpha\n"
	if err := os.WriteFile(filepath.Join(dir, applog.DaemonLogFile), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	var access []byte
	for i, e := range []applog.AccessEntry{
		{RequestID: "r200", Method: "GET", Host: "alpha.x", Path: "/", App: "alpha", Status: 200, User: "alice"},
		{RequestID: "r404", Method: "GET", Host: "alpha.x", Path: "/nope", App: "alpha", Status: 404, User: "bob"},
	} {
		e.Time = at.Add(time.Duration(i) * time.Second).UTC()
		b, _ := json.Marshal(e)
		access = append(append(access, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, applog.AccessLogFile), access, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCLI_LogsSources(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))
	writeOwnLogs(t, home)

	out, _, err := runCLI(t, home, "logs", "--source", "daemon")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"outpost INFO supervising 1 service(s)", "supervisor alpha WARN alpha: unhealthy", "tsnet alpha INFO"} {
		if !strings.Contains(out, want) {
			t.Errorf("--source daemon lacks %q:\n%s", want, out)
		}
	}
	out, _, err = runCLI(t, home, "logs", "--source", "tailnet", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "\n") != 1 || !strings.Contains(out, "tsnet alpha: ") {
		t.Errorf("--source tailnet --raw:\n%q", out)
	}
	out, _, err = runCLI(t, home, "logs", "--source", "access", "--status", "4xx", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rec applog.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil || rec.Fields["request_id"] != "r404" || rec.Source != applog.SourceAccess {
		t.Errorf("--status 4xx --json = %q (%v)", out, err)
	}
	out, _, err = runCLI(t, home, "logs", "alpha", "--source", "access", "--user", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "GET alpha.x/ 200") || strings.Contains(out, "/nope") {
		t.Errorf("--user alice:\n%s", out)
	}

	for _, bad := range [][]string{
		{"logs", "--source", "secrets"},
		{"logs", "--source", "daemon", "--all"},
		{"logs", "--source", "events", "--events", "alpha"},
		{"logs", "--source", "access", "--status", "40"},
		{"logs", "--source", "access", "--until", "whenever"},
		// The feed is the daemon's to give, and none is running here.
		{"logs", "--source", "events"},
	} {
		if _, _, err := runCLI(t, home, bad...); err == nil {
			t.Errorf("%v was accepted", bad)
		}
	}
}

// The gateway's source reads Outpost's own logs too, and the activity feed
// from the supervisor.
func TestLogsSource_OwnLogs(t *testing.T) {
	home := t.TempDir()
	writeOwnLogs(t, home)
	at := time.Now()
	src := logsSource{dir: filepath.Join(home, "logs"), services: listOnly{{Name: "alpha"}},
		events: func(_ context.Context, service string, limit int) ([]supervisor.EventRecord, error) {
			return []supervisor.EventRecord{{Service: "alpha", Kind: "exited", Message: "alpha exited 1", At: at}}, nil
		}}
	recs, err := src.Source(context.Background(), applog.SourceEvents, "", applog.Query{})
	if err != nil || len(recs) != 1 || recs[0].Level != applog.LevelWarn || recs[0].Source != applog.SourceEvents {
		t.Errorf("events = %+v, %v", recs, err)
	}
	recs, err = src.Source(context.Background(), applog.SourceAll, "alpha", applog.Query{})
	if err != nil || len(recs) != 5 {
		t.Errorf("all about alpha = %d records, %v", len(recs), err)
	}
}

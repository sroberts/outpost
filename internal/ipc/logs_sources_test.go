package ipc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
)

// The logs command reads Outpost's own logs by source, a service name then
// keeping the lines about it rather than naming a file.
func TestLogs_Sources(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	daemon := "time=" + at.Format(time.RFC3339) + " level=warn prefix=supervisor msg=unhealthy service=web\n" +
		"time=" + at.Add(time.Second).Format(time.RFC3339) + " level=info msg=hello\n"
	if err := os.WriteFile(filepath.Join(dir, applog.DaemonLogFile), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(applog.AccessEntry{Time: at, RequestID: "r1", Method: "GET", Host: "web.x", Path: "/", App: "web", Status: 503})
	if err := os.WriteFile(filepath.Join(dir, applog.AccessLogFile), append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	c := logsDaemon(t, dir)

	if got, err := c.Logs("", LogsRequest{Source: applog.SourceDaemon}); err != nil || len(got) != 2 {
		t.Errorf("daemon: %+v, %v", got, err)
	}
	if got, err := c.Logs("web", LogsRequest{Source: applog.SourceDaemon}); err != nil || len(got) != 1 || got[0].Component != "supervisor" {
		t.Errorf("daemon about web: %+v, %v", got, err)
	}
	if got, err := c.Logs("", LogsRequest{Source: applog.SourceAccess, Status: "5xx"}); err != nil || len(got) != 1 || got[0].Fields["request_id"] != "r1" {
		t.Errorf("access 5xx: %+v, %v", got, err)
	}
	// The activity feed comes from the supervisor; an idle one has none.
	if got, err := c.Logs("", LogsRequest{Source: applog.SourceEvents}); err != nil || len(got) != 0 {
		t.Errorf("events: %+v, %v", got, err)
	}
	if _, err := c.Logs("", LogsRequest{Source: "secrets"}); err == nil {
		t.Error("an unknown source was read")
	}
}

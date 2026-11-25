package applog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/logging"
)

// The components a text line's prefix is read from are the daemon's own.
func TestDaemonComponentsMatchConfig(t *testing.T) {
	if !reflect.DeepEqual(daemonComponents, config.LogComponents) {
		t.Errorf("daemonComponents = %v; config.LogComponents = %v", daemonComponents, config.LogComponents)
	}
}

// writeDaemonLog writes lines through the daemon's own logger in format, so
// the test reads exactly what the daemon writes.
func writeDaemonLog(t *testing.T, format string) string {
	t.Helper()
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, DaemonLogFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	root := logging.New(f, "debug", format)
	root.Info("outpost v1 starting")
	gw := logging.Component(root, "gateway", "debug")
	gw.Debug("GET yarr.x/feeds -> 401", "request_id", "abc123", "host", "yarr.x", "status", 401, "refused", "not signed in")
	sup := logging.Component(root, "supervisor", "debug")
	sup.Warn("yarr: unhealthy: a=b thing", "service", "yarr", "err", errors.New("line one\nline two"))
	ts := logging.Component(root, "tsnet", "debug")
	ts.Info("wiki: on the tailnet", "service", "wiki", "user", "alice")
	root.Error("portal: bad thing")
	return dir
}

func TestReadDaemon_EveryFormat(t *testing.T) {
	for _, format := range []string{"text", "json", "logfmt"} {
		t.Run(format, func(t *testing.T) {
			dir := writeDaemonLog(t, format)
			recs, err := ReadDaemon(dir, Query{})
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != 5 {
				for _, r := range recs {
					t.Logf("%+v", r)
				}
				t.Fatalf("read %d entries; want 5", len(recs))
			}
			for _, r := range recs {
				if r.Source != SourceDaemon || r.Time.IsZero() || r.Level == "" {
					t.Errorf("entry lacks its basics: %+v", r)
				}
			}
			if r := recs[0]; r.Message != "outpost v1 starting" || r.Component != "" || r.Level != LevelInfo {
				t.Errorf("root line = %+v", r)
			}
			if r := recs[1]; r.Component != "gateway" || r.Level != LevelDebug || r.Message != "GET yarr.x/feeds -> 401" ||
				r.Fields["request_id"] != "abc123" || r.Fields["status"] != "401" || r.Fields["refused"] != "not signed in" {
				t.Errorf("gateway line = %+v", r)
			}
			if r := recs[2]; r.Component != "supervisor" || r.Service != "yarr" || r.Level != LevelWarn ||
				r.Message != "yarr: unhealthy: a=b thing" || r.Fields["err"] != "line one\nline two" {
				t.Errorf("supervisor line = %+v", r)
			}
			// "portal:" begins the message; it isn't a part of the daemon.
			if r := recs[4]; r.Component != "" || r.Level != LevelError || r.Message != "portal: bad thing" {
				t.Errorf("error line = %+v", r)
			}

			if got := mustRead(t, dir, Query{Component: tailnetComponent}); len(got) != 1 || got[0].Service != "wiki" {
				t.Errorf("tsnet = %+v", got)
			}
			if got := mustRead(t, dir, Query{Service: "yarr"}); len(got) != 1 || got[0].Component != "supervisor" {
				t.Errorf("about yarr = %+v", got)
			}
			if got := mustRead(t, dir, Query{Level: LevelWarn}); len(got) != 2 {
				t.Errorf("warn and up = %+v", got)
			}
			if got := mustRead(t, dir, Query{Status: "4xx"}); len(got) != 1 || got[0].Fields["request_id"] != "abc123" {
				t.Errorf("4xx = %+v", got)
			}
			if got := mustRead(t, dir, Query{User: "alice"}); len(got) != 1 {
				t.Errorf("alice = %+v", got)
			}
			if got := mustRead(t, dir, Query{Apps: []string{"yarr"}}); len(got) != 1 || got[0].Service != "yarr" {
				t.Errorf("scoped to yarr = %+v", got)
			}
			if got := mustRead(t, dir, Query{Contains: "ABC123"}); len(got) != 1 {
				t.Errorf("containing the request id = %+v", got)
			}
		})
	}
}

func mustRead(t *testing.T, dir string, q Query) []Record {
	t.Helper()
	recs, err := ReadDaemon(dir, q)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// A missing log is an empty one: the daemon hasn't written it yet.
func TestReadOwn_Missing(t *testing.T) {
	for _, read := range []func(string, Query) ([]Record, error){ReadDaemon, ReadAccess} {
		recs, err := read(t.TempDir(), Query{})
		if err != nil || recs == nil || len(recs) != 0 {
			t.Errorf("recs = %v, err = %v", recs, err)
		}
	}
}

func writeAccess(t *testing.T, dir string, at time.Time, entries ...AccessEntry) {
	t.Helper()
	var b []byte
	for i, e := range entries {
		e.Time = at.Add(time.Duration(i) * time.Second)
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b = append(append(b, line...), '\n')
	}
	if err := os.WriteFile(filepath.Join(dir, AccessLogFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadAccess_Filters(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	writeAccess(t, dir, at,
		AccessEntry{RequestID: "a1", Method: "GET", Host: "yarr.x", Path: "/", App: "yarr", Status: 200, User: "alice"},
		AccessEntry{RequestID: "a2", Method: "GET", Host: "yarr.x", Path: "/api", App: "yarr", Status: 401, Token: "7", Refused: "access token rejected"},
		AccessEntry{RequestID: "a3", Method: "POST", Host: "portal.x", Path: "/login", Status: 303, User: "root"},
		AccessEntry{RequestID: "a4", Method: "GET", Host: "wiki.x", Path: "/", App: "wiki", Status: 502, User: "alice"},
	)
	read := func(q Query) string {
		t.Helper()
		recs, err := ReadAccess(dir, q)
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, r := range recs {
			ids = append(ids, r.Fields["request_id"])
		}
		return strings.Join(ids, ",")
	}
	for _, c := range []struct {
		q    Query
		want string
	}{
		{Query{}, "a1,a2,a3,a4"},
		{Query{Status: "4xx"}, "a2"},
		{Query{Status: "502"}, "a4"},
		{Query{Level: LevelWarn}, "a2,a4"},
		{Query{User: "alice"}, "a1,a4"},
		{Query{User: "7"}, "a2"},
		{Query{Service: "yarr"}, "a1,a2"},
		{Query{Apps: []string{"wiki"}}, "a4"},
		{Query{Since: at.Add(time.Second)}, "a3,a4"},
		{Query{Until: at.Add(2 * time.Second)}, "a1,a2"},
		{Query{Lines: 1}, "a4"},
	} {
		if got := read(c.q); got != c.want {
			t.Errorf("%+v: %s; want %s", c.q, got, c.want)
		}
	}
	recs, _ := ReadAccess(dir, Query{Lines: 1})
	if r := recs[0]; r.Message != "GET wiki.x/ 502" || r.Level != LevelError || r.Service != "wiki" || r.Source != SourceAccess {
		t.Errorf("record = %+v", r)
	}
}

func TestStatusFilters(t *testing.T) {
	for f, want := range map[string]bool{"404": true, "5xx": true, "2XX": true, "40": false, "600": false, "abc": false, "4x4": false, "": false} {
		if got := ValidStatus(f); got != want {
			t.Errorf("ValidStatus(%q) = %v", f, got)
		}
	}
	if !StatusMatches("404", "4xx") || StatusMatches("500", "4xx") || !StatusMatches("404", "404") || StatusMatches("", "4xx") {
		t.Error("StatusMatches")
	}
}

// Every source at once interleaves by time and keeps the newest; the
// activity feed is read newest first, and comes back oldest first.
func TestReader_All(t *testing.T) {
	dir := t.TempDir()
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	writeAccess(t, dir, at.Add(2*time.Second), AccessEntry{RequestID: "r1", Method: "GET", Host: "yarr.x", Path: "/", App: "yarr", Status: 200})
	if err := os.WriteFile(filepath.Join(dir, "yarr.log"), Format(at.Add(time.Second), Stdout, "yarr r1 served"), 0o600); err != nil {
		t.Fatal(err)
	}
	daemon := "time=" + at.Add(3*time.Second).Format(time.RFC3339) + " level=info prefix=gateway msg=hi request_id=r1\n"
	if err := os.WriteFile(filepath.Join(dir, DaemonLogFile), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	rd := Reader{
		Dir:      dir,
		Services: func(context.Context) ([]string, error) { return []string{"yarr"}, nil },
		Events: func(_ context.Context, _ string, _ int) ([]Record, error) {
			return []Record{FromEvent("yarr", "exited", "yarr exited", at.Add(4*time.Second)), FromEvent("yarr", "started", "yarr started", at)}, nil
		},
	}
	recs, err := rd.Read(context.Background(), SourceAll, "", Query{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.Source)
	}
	if strings.Join(got, ",") != "events,app,access,daemon,events" {
		t.Errorf("sources in order = %v", got)
	}
	if recs, _ := rd.Read(context.Background(), SourceAll, "", Query{Contains: "r1"}); len(recs) != 3 {
		t.Errorf("one request across sources = %+v", recs)
	}
	if recs, _ := rd.Read(context.Background(), SourceEvents, "", Query{Level: LevelWarn}); len(recs) != 1 || recs[0].Message != "yarr exited" {
		t.Errorf("warn events = %+v", recs)
	}
	if _, err := (Reader{Dir: dir}).Read(context.Background(), SourceEvents, "", Query{}); !errors.Is(err, ErrNoEvents) {
		t.Errorf("events without a feed: %v", err)
	}
	if _, err := rd.Read(context.Background(), "secrets", "", Query{}); err == nil {
		t.Error("an unknown source was read")
	}
	if w := Where(recs[0]); w != "" && !strings.Contains(w, "yarr") {
		t.Errorf("Where = %q", w)
	}
}

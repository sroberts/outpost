package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/auth"
)

// fakeSources is fakeLogs that also reads Outpost's own logs, from the same
// directory, as the daemon's source does.
type fakeSources struct {
	*fakeLogs
	events []applog.Record // newest first, as the feed is kept
}

func (f fakeSources) Source(ctx context.Context, source, service string, q applog.Query) ([]applog.Record, error) {
	rd := applog.Reader{
		Dir:      f.dir,
		Services: func(context.Context) ([]string, error) { return f.names, nil },
		Events: func(_ context.Context, service string, limit int) ([]applog.Record, error) {
			var out []applog.Record
			for _, e := range f.events {
				if service == "" || e.Service == service {
					out = append(out, e)
				}
			}
			return out, nil
		},
	}
	return rd.Read(ctx, source, service, q)
}

// sourcesHarness is logsHarness with a daemon log, an access log and an
// activity feed, each with lines about yarr, about wiki, and about neither.
func sourcesHarness(t *testing.T) *harness {
	t.Helper()
	h, f := logsHarness(t)
	at := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	daemon := strings.Join([]string{
		`time=` + at.Format(time.RFC3339) + ` level=info msg="supervising 2 service(s)"`,
		`time=` + at.Add(time.Second).Format(time.RFC3339) + ` level=warn prefix=supervisor msg="yarr: unhealthy" service=yarr`,
		`time=` + at.Add(2*time.Second).Format(time.RFC3339) + ` level=info prefix=tsnet msg="wiki: on the tailnet" service=wiki`,
		`time=` + at.Add(3*time.Second).Format(time.RFC3339) + ` level=debug prefix=gateway msg="GET yarr.outpost.test/ -> 502" request_id=feedc0de host=yarr.outpost.test status=502 user=root`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(f.dir, applog.DaemonLogFile), []byte(daemon), 0o600); err != nil {
		t.Fatal(err)
	}
	var access []byte
	for i, e := range []applog.AccessEntry{
		{RequestID: "feedc0de", Method: "GET", Host: "yarr.outpost.test", Path: "/", App: "yarr", Status: 502, IP: "127.0.0.1", User: "root"},
		{RequestID: "0ddba11", Method: "GET", Host: "wiki.outpost.test", Path: "/edit", App: "wiki", Status: 200, IP: "127.0.0.1", User: "alice", Token: "7"},
		{RequestID: "5ca1ab1e", Method: "POST", Host: "outpost.test", Path: "/login", Status: 303, IP: "127.0.0.1", User: "root"},
	} {
		e.Time = at.Add(time.Duration(i) * time.Second)
		b, _ := json.Marshal(e)
		access = append(append(access, b...), '\n')
	}
	if err := os.WriteFile(filepath.Join(f.dir, applog.AccessLogFile), access, 0o600); err != nil {
		t.Fatal(err)
	}
	h.g.logs = fakeSources{fakeLogs: f, events: []applog.Record{
		applog.FromEvent("", "sign-in", "root signed in", at.Add(2*time.Second)),
		applog.FromEvent("wiki", "exited", "wiki exited 1", at.Add(time.Second)),
		applog.FromEvent("yarr", "started", "yarr started (id 4)", at),
	}}
	return h
}

func TestLogsAPI_Sources(t *testing.T) {
	h := sourcesHarness(t)
	tok := h.pat("root")
	get := func(query string) logsReply {
		t.Helper()
		resp := h.send(http.MethodGet, portal+logsAPIPath+"?"+query, bearer(tok))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", query, resp.StatusCode)
		}
		return decodeLogs(t, resp)
	}

	if got := messages(get("source=daemon").Logs); !strings.Contains(got, ":supervising 2 service(s)") || !strings.Contains(got, "yarr:yarr: unhealthy") {
		t.Errorf("daemon = %s", got)
	}
	if lr := get("source=tailnet"); len(lr.Logs) != 1 || lr.Logs[0].Component != "tsnet" || lr.Logs[0].Service != "wiki" {
		t.Errorf("tailnet = %+v", lr.Logs)
	}
	if lr := get("source=access&status=5xx"); len(lr.Logs) != 1 || lr.Logs[0].Fields["request_id"] != "feedc0de" || lr.Logs[0].Level != applog.LevelError {
		t.Errorf("access 5xx = %+v", lr.Logs)
	}
	if lr := get("source=access&user=7"); len(lr.Logs) != 1 || lr.Logs[0].Service != "wiki" {
		t.Errorf("access by token 7 = %+v", lr.Logs)
	}
	if got := messages(get("source=events&level=warn").Logs); got != "wiki:wiki exited 1" {
		t.Errorf("events at warn = %s", got)
	}
	// One request, followed by its id through every source it's in.
	lr := get("source=all&q=feedc0de")
	if len(lr.Logs) != 2 || lr.Logs[0].Source == lr.Logs[1].Source {
		t.Errorf("all for one request = %+v", lr.Logs)
	}
	// Everything, interleaved: the apps' own lines are there too.
	if got := messages(get("source=all").Logs); !strings.Contains(got, "yarr:yarr started") || !strings.Contains(got, "wiki:wiki slow") || !strings.Contains(got, "root signed in") {
		t.Errorf("all = %s", got)
	}
	for _, bad := range []string{"source=secrets", "source=access&status=40", "source=access&status=hello", "until=yesterday"} {
		if resp := h.send(http.MethodGet, portal+logsAPIPath+"?"+bad, bearer(tok)); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", bad, resp.StatusCode)
		}
	}
	// The page's Download link is the same lines as a file.
	resp := h.send(http.MethodGet, portal+logsAPIPath+"?source=access&download=1", bearer(tok))
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q", cd)
	}
}

// A token scoped to apps sees only the lines about them, from every source:
// nothing of another app's, the portal's, or the daemon's own.
func TestLogsAPI_ScopedTokenSeesOnlyItsApps(t *testing.T) {
	h := sourcesHarness(t)
	scoped := h.pat("root", "yarr")
	for _, source := range applog.Sources {
		resp := h.send(http.MethodGet, portal+logsAPIPath+"?source="+source, bearer(scoped))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", source, resp.StatusCode)
		}
		lr := decodeLogs(t, resp)
		for _, rec := range lr.Logs {
			if rec.Service != "yarr" {
				t.Errorf("%s: a scoped token read %+v", source, rec)
			}
		}
		if len(lr.Logs) == 0 && source != applog.SourceTailnet {
			t.Errorf("%s: nothing about yarr", source)
		}
	}
	// Naming another app reads as a bad token, as it does for the apps' output.
	assertJSONRefused(t, h.send(http.MethodGet, portal+logsAPIPath+"?source=access&service=wiki", bearer(scoped)))
	assertJSONRefused(t, h.send(http.MethodGet, portal+logsAPIPath+"?source=daemon&service=wiki", bearer(scoped)))
}

// Without a way to read Outpost's own logs, the page offers only the apps'
// output, and asking for another source is refused.
func TestLogsPage_SourcesOnlyWhenKept(t *testing.T) {
	h, _ := logsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	page := body(t, h.get(c, portal+"/logs"))
	if strings.Contains(page, `name="source"`) || strings.Contains(page, "logs.js") {
		t.Error("the page offers sources it can't read")
	}
	if resp := h.get(c, portal+"/logs?source=daemon"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("source=daemon without sources: status %d", resp.StatusCode)
	}
}

func TestLogsPage_Sources(t *testing.T) {
	h := sourcesHarness(t)
	h.addUser(auth.User{Username: "ops", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")

	page := body(t, h.get(c, portal+"/logs"))
	for _, want := range []string{`<select id="log-source" name="source">`, `<option value="access"`, "Follow new lines", "logs.js", "Download"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	page = body(t, h.get(c, portal+"/logs?source=access&status=5xx"))
	for _, want := range []string{"GET yarr.outpost.test/ 502", "request · Yarr", "request feedc0de", `href="/logs?q=feedc0de&amp;source=all#newest"`} {
		if !strings.Contains(page, want) {
			t.Errorf("access page lacks %q", want)
		}
	}
	if strings.Contains(page, "/login 303") {
		t.Error("the status filter let a 303 through")
	}
	page = body(t, h.get(c, portal+"/logs?source=all&q=feedc0de"))
	if !strings.Contains(page, "gateway") || !strings.Contains(page, "request · Yarr") {
		t.Errorf("one request's lines:\n%s", page)
	}
	if resp := h.get(c, portal+"/logs?source=access&until=2026-11-18T09:00"); resp.StatusCode != http.StatusOK {
		t.Errorf("a time field's value: status %d", resp.StatusCode)
	}
}

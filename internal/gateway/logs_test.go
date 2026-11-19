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
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeLogs reads real log files from a directory, as the daemon's source
// does, for the services the harness declares.
type fakeLogs struct {
	dir   string
	names []string
	asked []string // every service read, "" for all
}

func (f *fakeLogs) Logs(_ context.Context, service string, q applog.Query) ([]applog.Record, error) {
	f.asked = append(f.asked, service)
	if service == "" {
		return applog.ReadAll(f.dir, f.names, q)
	}
	recs, err := applog.Read(f.dir, service, q)
	if err == applog.ErrNoLog {
		return []applog.Record{}, nil
	}
	return recs, err
}

const evilLine = `<script>alert("owned")</script>`

// logsHarness declares yarr and wiki, gives each a log, and adds root, an
// admin, and alice, who isn't one.
func logsHarness(t *testing.T) (*harness, *fakeLogs) {
	t.Helper()
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Title: "Yarr", Status: "running"},
		supervisor.ServiceStatus{Name: "wiki", Status: "stopped"},
	)
	dir := t.TempDir()
	base := time.Now().Add(-10 * time.Minute).UTC()
	write := func(name string, lines ...string) {
		var b []byte
		for i, l := range lines {
			stream := applog.Stdout
			if strings.Contains(l, "error") {
				stream = applog.Stderr
			}
			b = append(b, applog.Format(base.Add(time.Duration(i)*time.Second), stream, l)...)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".log"), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("yarr", "yarr started", `{"level":"error","msg":"feed failed","feed":"hn"}`, evilLine)
	write("wiki", "level=warn msg=\"wiki slow\"", "wiki page saved")
	f := &fakeLogs{dir: dir, names: []string{"yarr", "wiki"}}
	h.g.logs = f
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

type logsReply struct {
	Logs  []applog.Record `json:"logs"`
	Error string          `json:"error"`
}

func decodeLogs(t *testing.T, resp *http.Response) logsReply {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	var lr logsReply
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatal(err)
	}
	return lr
}

// assertJSONRefused is assertRefused for Outpost's own API, which says so in
// JSON rather than plain text.
func assertJSONRefused(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d; want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
		t.Errorf("WWW-Authenticate = %q", got)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("redirected to %s", loc)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	if len(resp.Cookies()) != 0 {
		t.Errorf("set cookies: %v", resp.Cookies())
	}
}

func messages(recs []applog.Record) string {
	var s []string
	for _, r := range recs {
		s = append(s, r.Service+":"+r.Message)
	}
	return strings.Join(s, " | ")
}

func TestLogsPage_AdminReadsEscapedLines(t *testing.T) {
	h, _ := logsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	home := body(t, h.get(c, portal+"/"))
	if !strings.Contains(home, `href="/logs"`) {
		t.Error("the catalog doesn't link an admin to the Logs page")
	}
	resp := h.get(c, portal+"/logs")
	page := body(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d\n%s", resp.StatusCode, page)
	}
	for _, want := range []string{"yarr started", "feed failed", "feed=hn", "wiki slow", "Yarr", `<option value="wiki"`} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, evilLine) || !strings.Contains(page, "&lt;script&gt;") {
		t.Error("a log line reached the page unescaped")
	}
	// Oldest first: the newest line is at the bottom.
	if strings.Index(page, "yarr started") > strings.Index(page, "&lt;script&gt;") {
		t.Error("lines aren't oldest first")
	}

	resp = h.get(c, portal+"/logs?service=wiki&level=warn")
	page = body(t, resp)
	if !strings.Contains(page, "wiki slow") || strings.Contains(page, "wiki page saved") || strings.Contains(page, "yarr started") {
		t.Errorf("filtered page:\n%s", page)
	}
	if resp := h.get(c, portal+"/logs?level=loud"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad level: status %d", resp.StatusCode)
	}
	if resp := h.get(c, portal+"/logs?service=..%2Fetc"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown service: status %d", resp.StatusCode)
	}
}

func TestLogsPage_RefusesOthers(t *testing.T) {
	h, _ := logsHarness(t)
	resp := h.get(h.client(false), portal+"/logs")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Errorf("anonymous: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	c := h.client(true)
	h.login(c, "alice", "", "/")
	resp = h.get(c, portal+"/logs")
	page := body(t, resp)
	if resp.StatusCode != http.StatusForbidden || strings.Contains(page, "yarr started") {
		t.Errorf("non-admin: %d", resp.StatusCode)
	}
	if strings.Contains(body(t, h.get(c, portal+"/")), `href="/logs"`) {
		t.Error("a non-admin is linked to the Logs page")
	}
}

func TestLogs_AbsentWithoutASource(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	for _, u := range []string{portal + "/logs", portal + logsAPIPath} {
		if resp := h.get(c, u); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", u, resp.StatusCode)
		}
	}
	if strings.Contains(body(t, h.get(c, portal+"/")), `href="/logs"`) {
		t.Error("the catalog links a page that isn't there")
	}
}

func TestLogsAPI_Session(t *testing.T) {
	h, _ := logsHarness(t)
	if resp := h.get(h.client(false), portal+logsAPIPath); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", resp.StatusCode)
	} else if lr := decodeLogs(t, resp); lr.Error == "" {
		t.Error("anonymous: no JSON error")
	}

	alice := h.client(true)
	h.login(alice, "alice", "", "/")
	if resp := h.get(alice, portal+logsAPIPath); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin: %d", resp.StatusCode)
	}

	c := h.client(true)
	h.login(c, "root", "", "/")
	resp := h.get(c, portal+logsAPIPath+"?lines=2")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin: %d", resp.StatusCode)
	}
	for k, v := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff"} {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q", k, got)
		}
	}
	lr := decodeLogs(t, resp)
	if got := messages(lr.Logs); got != "wiki:wiki page saved | yarr:"+evilLine {
		t.Errorf("logs = %s", got)
	}

	for _, q := range []string{"level=loud", "stream=both", "lines=0", "lines=x", "since=yesterday"} {
		resp := h.get(c, portal+logsAPIPath+"?"+q)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, resp.StatusCode)
		} else if decodeLogs(t, resp).Error == "" {
			t.Errorf("%s: no JSON error", q)
		}
	}
	if resp := h.get(c, portal+logsAPIPath+"?service=nope"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown service: %d", resp.StatusCode)
	}
	lr = decodeLogs(t, h.get(c, portal+logsAPIPath+"?service=yarr&level=error"))
	if got := messages(lr.Logs); got != "yarr:feed failed" {
		t.Errorf("level=error: %s", got)
	}
	if resp := h.post(c, portal+logsAPIPath, portal, nil); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", resp.StatusCode)
	}
	// Only on the portal.
	if resp := h.get(c, yarr+logsAPIPath); resp.StatusCode == http.StatusOK {
		t.Error("an app host answers the logs API")
	}
}

func TestLogsAPI_Tokens(t *testing.T) {
	h, f := logsHarness(t)

	resp := h.send(http.MethodGet, portal+logsAPIPath, bearer(h.pat("root")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin token: %d", resp.StatusCode)
	}
	if lr := decodeLogs(t, resp); len(lr.Logs) != 5 {
		t.Errorf("admin token read %d lines", len(lr.Logs))
	}

	if resp := h.send(http.MethodGet, portal+logsAPIPath, bearer(h.pat("alice"))); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin token: %d", resp.StatusCode)
	}

	// A bad token beside a good admin session is still a bad token.
	c := h.client(false)
	h.login(c, "root", "", "/")
	resp = h.get(c, portal+logsAPIPath, "Authorization", "Bearer "+auth.AccessTokenPrefix+"wrong")
	assertJSONRefused(t, resp)
	if decodeLogs(t, resp).Error == "" {
		t.Error("bad token: no JSON error")
	}

	// An internet token is for one app's Funnel, not the portal.
	secret, _, err := h.store.CreateInternetAccessToken("root", "net", "yarr", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONRefused(t, h.send(http.MethodGet, portal+logsAPIPath, bearer(secret)))

	// A token scoped to yarr reads yarr, and nothing else, not even by
	// asking for everything.
	scoped := h.pat("root", "yarr")
	lr := decodeLogs(t, h.send(http.MethodGet, portal+logsAPIPath+"?service=yarr", bearer(scoped)))
	if len(lr.Logs) != 3 {
		t.Errorf("scoped, its own app: %s", messages(lr.Logs))
	}
	f.asked = nil
	lr = decodeLogs(t, h.send(http.MethodGet, portal+logsAPIPath, bearer(scoped)))
	if got := messages(lr.Logs); strings.Contains(got, "wiki") || len(lr.Logs) != 3 {
		t.Errorf("scoped, everything: %s", got)
	}
	if strings.Join(f.asked, ",") != "yarr" {
		t.Errorf("scoped token read %q", f.asked)
	}
	assertJSONRefused(t, h.send(http.MethodGet, portal+logsAPIPath+"?service=wiki", bearer(scoped)))
}

// mcpCall posts one JSON-RPC message to Outpost's MCP server.
func mcpCall(h *harness, hdr http.Header, msg string) *http.Response {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, portal+ownMCPPath, strings.NewReader(msg))
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, vs := range hdr {
		req.Header[k] = vs
	}
	resp, err := h.client(false).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

type rpcReplyBody struct {
	ID     json.RawMessage `json:"id"`
	Result struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
		Content []struct {
			Type, Text string
		} `json:"content"`
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	} `json:"result"`
	Error *rpcError `json:"error"`
}

func decodeRPC(t *testing.T, resp *http.Response) rpcReplyBody {
	t.Helper()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var r rpcReplyBody
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOwnMCP(t *testing.T) {
	h, _ := logsHarness(t)
	tok := bearer(h.pat("root"))

	r := decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`))
	if r.Error != nil || r.Result.ProtocolVersion != "2025-06-18" || r.Result.ServerInfo.Name != "outpost" || string(r.ID) != "1" {
		t.Errorf("initialize = %+v", r)
	}
	if resp := mcpCall(h, tok, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: %d", resp.StatusCode)
	}
	if r := decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":"p","method":"ping"}`)); r.Error != nil {
		t.Errorf("ping: %+v", r.Error)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	var names []string
	for _, tl := range r.Result.Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "list_services,read_logs,recent_errors" {
		t.Errorf("tools = %v", names)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_logs","arguments":{"service":"yarr","lines":2}}}`))
	if r.Error != nil || r.Result.IsError || len(r.Result.Content) != 1 {
		t.Fatalf("read_logs = %+v", r)
	}
	text := r.Result.Content[0].Text
	if lines := strings.Split(text, "\n"); len(lines) != 2 || !strings.Contains(lines[0], "ERRO feed failed feed=hn") || !strings.Contains(lines[1], evilLine) {
		t.Errorf("read_logs text = %q", text)
	}
	if !strings.Contains(string(r.Result.StructuredContent), `"logs"`) {
		t.Errorf("structuredContent = %s", r.Result.StructuredContent)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"read_logs","arguments":{}}}`))
	if text := r.Result.Content[0].Text; !strings.Contains(text, "[wiki] ") || !strings.Contains(text, "[yarr] ") {
		t.Errorf("every service, unprefixed: %q", text)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"recent_errors","arguments":{}}}`))
	if text := r.Result.Content[0].Text; !strings.Contains(text, "feed failed") || strings.Contains(text, "started") {
		t.Errorf("recent_errors = %q", text)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"list_services","arguments":{}}}`))
	if text := r.Result.Content[0].Text; !strings.Contains(text, "yarr (Yarr): running") || !strings.Contains(text, "wiki (Wiki): stopped") {
		t.Errorf("list_services = %q", text)
	}

	r = decodeRPC(t, mcpCall(h, tok, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read_logs","arguments":{"service":"nope"}}}`))
	if r.Error != nil || !r.Result.IsError {
		t.Errorf("unknown service: %+v", r)
	}
	for msg, code := range map[string]int{
		`{"jsonrpc":"2.0","id":8,"method":"resources/list"}`:                                                         rpcMethodNotFound,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"rm_rf","arguments":{}}}`:                    rpcInvalidParams,
		`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"read_logs","arguments":{"level":"loud"}}}`: rpcInvalidParams,
		`{"jsonrpc":"2.0","id":11,"method":"tools/call","params":{"name":"read_logs","arguments":{"lines":1.5}}}`:    rpcInvalidParams,
		`[{"jsonrpc":"2.0","id":12,"method":"ping"}]`:                                                                rpcInvalidRequest,
		`{"jsonrpc":"1.0","id":13,"method":"ping"}`:                                                                  rpcInvalidRequest,
		`{"jsonrpc":`: rpcParseError,
	} {
		r := decodeRPC(t, mcpCall(h, tok, msg))
		if r.Error == nil || r.Error.Code != code {
			t.Errorf("%s: error %+v; want %d", msg, r.Error, code)
		}
	}
}

func TestOwnMCP_Refusals(t *testing.T) {
	h, _ := logsHarness(t)
	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	assertJSONRefused(t, mcpCall(h, nil, ping))
	assertJSONRefused(t, mcpCall(h, bearer(auth.AccessTokenPrefix+"wrong"), ping))
	if resp := mcpCall(h, bearer(h.pat("alice")), ping); resp.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin token: %d", resp.StatusCode)
	}

	// A session cookie is no way in, even an admin's.
	c := h.client(false)
	h.login(c, "root", "", "/")
	req, _ := http.NewRequest(http.MethodPost, portal+ownMCPPath, strings.NewReader(ping))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("session cookie: %d", resp.StatusCode)
	}

	tok := bearer(h.pat("root"))
	evil := http.Header{"Authorization": tok["Authorization"], "Origin": {"http://evil.example"}}
	if resp := mcpCall(h, evil, ping); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign Origin: %d", resp.StatusCode)
	}
	own := http.Header{"Authorization": tok["Authorization"], "Origin": {portal}}
	if r := decodeRPC(t, mcpCall(h, own, ping)); r.Error != nil {
		t.Errorf("portal Origin: %+v", r.Error)
	}
	if resp := h.send(http.MethodGet, portal+ownMCPPath, tok); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", resp.StatusCode)
	}

	// A token scoped to one app sees that app alone.
	scoped := bearer(h.pat("root", "yarr"))
	r := decodeRPC(t, mcpCall(h, scoped, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_services","arguments":{}}}`))
	if text := r.Result.Content[0].Text; strings.Contains(text, "wiki") {
		t.Errorf("scoped list_services = %q", text)
	}
	r = decodeRPC(t, mcpCall(h, scoped, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"read_logs","arguments":{"service":"wiki"}}}`))
	if !r.Result.IsError || strings.Contains(r.Result.Content[0].Text, "wiki slow") {
		t.Errorf("scoped read of another app = %+v", r.Result)
	}
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/version"
)

// Outpost's own MCP server: tools that read the services' state and output,
// for an admin's MCP client. It is not /mcp/<name>, which carries a client to
// an app's own MCP endpoint.
//
// It speaks the part of Streamable HTTP a stateless, tools-only server needs:
// each POST is one JSON-RPC message and gets one JSON reply, or 202 for a
// notification. There is no event stream to GET, no session, and no batch,
// which 2025-06-18 took out of the protocol.
const (
	ownMCPPath = "/.outpost/mcp"
	// ownMCPProtocol is the protocol version this server speaks first.
	ownMCPProtocol = "2025-06-18"
	// ownMCPMaxBody bounds a request: a tool call's arguments are a few
	// short strings.
	ownMCPMaxBody = 1 << 20
	// ownMCPMaxBytes bounds the log text in one tool result, which a model
	// has to read whole.
	ownMCPMaxBytes = 1 << 20
	// ownMCPDefaultLines is read_logs' default, less than the page's, for
	// the same reason.
	ownMCPDefaultLines = 100
)

// ownMCPProtocols are the versions whose shape matches what this server
// sends, so it can answer a client in its own; anything else is offered
// ownMCPProtocol, as the protocol says.
var ownMCPProtocols = map[string]bool{"2025-06-18": true, "2025-03-26": true, "2024-11-05": true}

// JSON-RPC error codes.
const (
	rpcParseError     = -32700
	rpcInvalidRequest = -32600
	rpcMethodNotFound = -32601
	rpcInvalidParams  = -32602
	rpcInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// serveOwnMCP answers POST /.outpost/mcp. It takes an admin's access token
// and nothing else, as /mcp/<name> does: an MCP client has no cookie a
// browser didn't give it, and a cookie would make a cross-site form post a
// way to call it. See logsCaller for which tokens work.
func (g *Gateway) serveOwnMCP(rw http.ResponseWriter, r *http.Request) {
	w := &jsonErrors{ResponseWriter: rw}
	apiHeaders(w.Header())
	if g.logs == nil {
		removeAccessTokens(r.Header)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		removeAccessTokens(r.Header)
		// No event stream to GET and no session to DELETE.
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A page on another origin that a browser was sent to, by DNS
	// rebinding or otherwise, says so in Origin; a client that isn't a
	// browser sends none.
	if o := r.Header.Get("Origin"); o != "" && !g.isPortalOrigin(r, o) {
		removeAccessTokens(r.Header)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	a, ok := g.logsCaller(w, r, "Outpost's MCP server", false)
	if !ok {
		return
	}
	a.passed()

	r.Body = http.MaxBytesReader(w, r.Body, ownMCPMaxBody)
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		g.rpcReply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcParseError, "parse error"}})
		return
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '[' {
		g.rpcReply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcInvalidRequest, "batches aren't supported"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		g.rpcReply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcInvalidRequest, "invalid request"}})
		return
	}
	// A message with no id is a notification, or a response to a request
	// this server never makes; either way it's taken and not answered.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" || !validRPCID(req.ID) {
		g.rpcReply(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{rpcInvalidRequest, "invalid request"}})
		return
	}
	resp := rpcResponse{ID: req.ID}
	resp.Result, resp.Error = g.ownMCPCall(r.Context(), a, req)
	g.rpcReply(w, resp)
}

// validRPCID is a string or a number, which is all JSON-RPC lets an id be
// that this server would answer.
func validRPCID(id json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(id, &v); err != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// isPortalOrigin reports whether an Origin header names the portal, as a
// page the portal served would send it.
func (g *Gateway) isPortalOrigin(r *http.Request, o string) bool {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	return origin(u.Scheme, u.Host) == origin(g.scheme, g.rawHost(r))
}

func (g *Gateway) rpcReply(w http.ResponseWriter, resp rpcResponse) {
	resp.JSONRPC = "2.0"
	body, err := json.Marshal(resp)
	if err != nil {
		g.logf("gateway: mcp: %v", err)
		body, _ = json.Marshal(rpcResponse{JSONRPC: "2.0", ID: resp.ID, Error: &rpcError{rpcInternalError, "internal error"}}) // can't fail: nothing in it but an id that came in as JSON
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n')) // a client that hung up has nothing to be told
}

func (g *Gateway) ownMCPCall(ctx context.Context, a logsAccess, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			if err := json.Unmarshal(req.Params, &p); err != nil {
				return nil, &rpcError{rpcInvalidParams, "invalid params"}
			}
		}
		v := ownMCPProtocol
		if ownMCPProtocols[p.ProtocolVersion] {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "outpost", "version": version.Get().Version},
			"instructions": "Outpost supervises this machine's services. list_services says what is declared and how each is doing; " +
				"read_logs reads a service's captured stdout and stderr, or every service's interleaved by time; recent_errors reads just the errors.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": ownMCPTools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{rpcInvalidParams, "invalid params"}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		switch p.Name {
		case "list_services":
			return g.toolListServices(ctx, a), nil
		case "read_logs":
			return g.toolReadLogs(ctx, a, args, false)
		case "recent_errors":
			return g.toolReadLogs(ctx, a, args, true)
		}
		return nil, &rpcError{rpcInvalidParams, "unknown tool: " + p.Name}
	}
	return nil, &rpcError{rpcMethodNotFound, "method not found: " + req.Method}
}

// readOnly is every tool's annotation: none of them changes anything.
var readOnly = map[string]any{"readOnlyHint": true, "openWorldHint": false}

func ownMCPTools() []map[string]any {
	service := map[string]any{"type": "string", "description": "A service's name, from list_services. Leave it out to read every service, interleaved by time."}
	since := map[string]any{"type": "string", "description": "Only lines written after this: an RFC 3339 time such as 2026-11-18T12:00:00Z, or a duration before now such as 15m or 2h."}
	lines := map[string]any{"type": "integer", "minimum": 1, "maximum": logsMaxLines, "description": "How many of the newest matching lines to return."}
	return []map[string]any{
		{
			"name":        "list_services",
			"title":       "List services",
			"description": "Lists the services Outpost supervises, with each one's name, title and status.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"annotations": readOnly,
		},
		{
			"name":        "read_logs",
			"title":       "Read logs",
			"description": "Reads the newest lines a service wrote to stdout and stderr, oldest first, one per line: local time, level, message and fields.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service":  service,
					"lines":    withDefault(lines, ownMCPDefaultLines),
					"level":    map[string]any{"type": "string", "enum": applog.Levels, "description": "Only lines at this level or above. Lines with no level count as info."},
					"contains": map[string]any{"type": "string", "description": "Only lines containing this text, ignoring case."},
					"stream":   map[string]any{"type": "string", "enum": []string{applog.Stdout, applog.Stderr}, "description": "Only lines from stdout (out) or stderr (err)."},
					"since":    since,
				},
			},
			"annotations": readOnly,
		},
		{
			"name":        "recent_errors",
			"title":       "Recent errors",
			"description": "Reads the error lines services wrote recently, oldest first. With no since, the last hour.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"service": service,
					"since":   since,
					"lines":   withDefault(lines, ownMCPDefaultLines),
				},
			},
			"annotations": readOnly,
		},
	}
}

func withDefault(schema map[string]any, v any) map[string]any {
	out := map[string]any{"default": v}
	for k, x := range schema {
		out[k] = x
	}
	return out
}

// toolResult is a tool's answer; isError carries a failure the model can
// read and act on, where a JSON-RPC error would be the client's problem.
func toolResult(text string, structured any, isError bool) map[string]any {
	res := map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
	if structured != nil {
		res["structuredContent"] = structured
	}
	if isError {
		res["isError"] = true
	}
	return res
}

func (g *Gateway) toolListServices(ctx context.Context, a logsAccess) map[string]any {
	svcs, err := g.logsServices(ctx, a)
	if err != nil {
		g.logf("gateway: mcp: list services: %v", err)
		return toolResult("Couldn't list the services.", nil, true)
	}
	var b strings.Builder
	for _, s := range svcs {
		fmt.Fprintf(&b, "%s (%s): %s\n", s.Name, s.Title, orWord(s.Status, "unknown"))
	}
	if len(svcs) == 0 {
		b.WriteString("No services are declared.")
	}
	return toolResult(strings.TrimSuffix(b.String(), "\n"), map[string]any{"services": svcs}, false)
}

func orWord(s, empty string) string {
	if s == "" {
		return empty
	}
	return s
}

// toolReadLogs is read_logs, and recent_errors when errorsOnly is set.
func (g *Gateway) toolReadLogs(ctx context.Context, a logsAccess, raw json.RawMessage, errorsOnly bool) (any, *rpcError) {
	var args struct {
		Service  string   `json:"service"`
		Lines    *float64 `json:"lines"`
		Level    string   `json:"level"`
		Contains string   `json:"contains"`
		Stream   string   `json:"stream"`
		Since    string   `json:"since"`
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, &rpcError{rpcInvalidParams, "invalid arguments: " + err.Error()}
	}
	lines := ownMCPDefaultLines
	if args.Lines != nil {
		n := *args.Lines
		if n < 1 || n != float64(int(n)) {
			return nil, &rpcError{rpcInvalidParams, "lines must be a whole number above 0"}
		}
		lines = int(min(n, logsMaxLines))
	}
	if errorsOnly {
		args.Level, args.Stream, args.Contains = applog.LevelError, "", ""
		if args.Since == "" {
			args.Since = "1h"
		}
	}
	q, err := logsQuery(lines, args.Level, args.Stream, args.Contains, args.Since)
	if err != nil {
		return nil, &rpcError{rpcInvalidParams, err.Error()}
	}
	lr := logsRequest{service: strings.TrimSpace(args.Service), q: q}
	// Outside a scoped token's apps reads as not there at all, so the token
	// can't be used to learn what else exists.
	if lr.service != "" && !a.reaches(lr.service) {
		return toolResult("There's no service named "+lr.service+". list_services lists them.", nil, true), nil
	}
	svcs, err := g.logsServices(ctx, a)
	if err != nil {
		g.logf("gateway: mcp: list services: %v", err)
		return toolResult("Couldn't list the services.", nil, true), nil
	}
	recs, err := g.readLogs(ctx, a, svcs, lr, ownMCPMaxBytes)
	if errors.Is(err, errNoSuchService) {
		return toolResult("There's no service named "+lr.service+". list_services lists them.", nil, true), nil
	}
	if err != nil {
		g.logf("gateway: mcp: logs: %v", err)
		return toolResult("Couldn't read the logs.", nil, true), nil
	}
	var b strings.Builder
	for _, rec := range recs {
		if lr.service == "" {
			b.WriteString("[" + rec.Service + "] ")
		}
		b.WriteString(applog.Text(rec))
		b.WriteByte('\n')
	}
	text := strings.TrimSuffix(b.String(), "\n")
	if len(recs) == 0 {
		text = "No matching lines."
		if errorsOnly {
			text = "No errors since " + q.Since.Local().Format(time.RFC3339) + "."
		}
	}
	return toolResult(text, map[string]any{"logs": recs}, false), nil
}

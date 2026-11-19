package ipc

import (
	"context"
	"errors"
	"time"

	"github.com/sroberts/outpost/internal/applog"
)

// CmdLogs reads services' captured output: what the CLI, the dashboard, the
// portal, its JSON API and its MCP server all show.
const CmdLogs = "logs"

// LogsRequest is the logs command's argument. With Request.Service empty it
// reads every service, interleaved by time.
type LogsRequest struct {
	Lines    int       `json:"lines,omitempty"`
	Since    time.Time `json:"since,omitzero"`
	Level    string    `json:"level,omitempty"`
	Stream   string    `json:"stream,omitempty"`
	Contains string    `json:"contains,omitempty"`
}

// maxLogBytes bounds one reply: lines can be up to 64 KiB each, and a reply
// is held whole in memory at both ends. The newest lines are the ones kept.
const maxLogBytes = 4 << 20

var errNoLogs = errors.New("this daemon doesn't serve logs")

// UseLogs enables the logs command, reading from dir. Call it before Serve.
func (s *Server) UseLogs(dir string) { s.logDir = dir }

func (s *Server) dispatchLogs(ctx context.Context, req Request) Response {
	if s.logDir == "" {
		return errorResponse(errNoLogs)
	}
	var lr LogsRequest
	if req.Logs != nil {
		lr = *req.Logs
	}
	q := applog.Query{Lines: lr.Lines, Since: lr.Since, Level: lr.Level, Stream: lr.Stream, Contains: lr.Contains}
	var recs []applog.Record
	var err error
	if req.Service != "" {
		recs, err = applog.Read(s.logDir, req.Service, q)
		if errors.Is(err, applog.ErrNoLog) {
			// Known but silent so far, or never run: an empty log, not a failure.
			return Response{OK: true, Logs: []applog.Record{}}
		}
	} else {
		var names []string
		list, lerr := s.sup.List(ctx)
		if lerr != nil {
			return errorResponse(lerr)
		}
		for _, st := range list {
			names = append(names, st.Name)
		}
		recs, err = applog.ReadAll(s.logDir, names, q)
	}
	if err != nil {
		return errorResponse(err)
	}
	return Response{OK: true, Logs: capLogBytes(recs, maxLogBytes)}
}

// capLogBytes keeps the newest records that fit in max bytes of text.
func capLogBytes(recs []applog.Record, max int) []applog.Record {
	total := 0
	for i := len(recs) - 1; i >= 0; i-- {
		total += len(recs[i].Line) + len(recs[i].Message)
		if total > max {
			return recs[i+1:]
		}
	}
	if recs == nil {
		return []applog.Record{}
	}
	return recs
}

// Logs reads a service's captured output, or every service's when service is
// "", newest q.Lines matching, oldest first.
func (c *Client) Logs(service string, q LogsRequest) ([]applog.Record, error) {
	resp, err := c.do(Request{Command: CmdLogs, Service: service, Logs: &q})
	if err != nil {
		return nil, err
	}
	return resp.Logs, nil
}

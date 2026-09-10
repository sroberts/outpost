package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/supervisor"
)

// maxSocketPath is the shortest of the platform sockaddr_un limits, so the
// check behaves the same on darwin and linux.
const maxSocketPath = 104

// Server exposes a Supervisor over a Unix socket.
type Server struct {
	sup  *supervisor.Supervisor
	path string
	ln   net.Listener
	logf func(string, ...any)

	wg sync.WaitGroup
}

// Listen binds the socket at path. A socket left behind by a dead daemon is
// removed; a live one is an error rather than something to steal.
func Listen(path string, sup *supervisor.Supervisor, logf func(string, ...any)) (*Server, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	// Unix domain sockets have a hard path limit in the kernel's sockaddr_un
	// (104 bytes on darwin, 108 on linux); without this the failure surfaces as
	// a bare "bind: invalid argument".
	if len(path) >= maxSocketPath {
		return nil, fmt.Errorf("socket path %s is %d bytes, over the %d-byte limit for unix sockets",
			path, len(path), maxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	if _, err := os.Stat(path); err == nil {
		if conn, err := net.DialTimeout("unix", path, time.Second); err == nil {
			conn.Close()
			return nil, fmt.Errorf("another outpost daemon is listening on %s", path)
		}
		// Stale socket from a daemon that did not clean up.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket %s: %w", path, err)
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}
	// The socket controls the user's services: keep it owner-only.
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return &Server{sup: sup, path: path, ln: ln, logf: logf}, nil
}

// Addr reports the socket path.
func (s *Server) Addr() string { return s.path }

// Serve accepts connections until ctx is cancelled or Close is called.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		s.ln.Close()
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				s.wg.Wait()
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// Close stops listening and removes the socket file.
func (s *Server) Close() error {
	err := s.ln.Close()
	os.Remove(s.path)
	return err
}

// handle serves one request/response exchange.
func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	// Reading the request is always quick; only running it can be slow.
	_ = conn.SetDeadline(time.Now().Add(RequestTimeout))

	body, err := io.ReadAll(io.LimitReader(conn, 1<<20))
	if err != nil {
		s.logf("outpost: read request: %v", err)
		return
	}
	req, err := decodeRequest(body)
	if err != nil {
		writeResponse(conn, errorResponse(fmt.Errorf("malformed request: %w", err)))
		return
	}

	// Now that the command is known, give it its own budget. A flat bound
	// cancelled a backup of a large storage directory partway through and
	// reported a failure for work that would have succeeded.
	budget := Timeout(req.Command)
	_ = conn.SetDeadline(time.Now().Add(budget))

	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	writeResponse(conn, s.dispatch(ctx, req))
}

// dispatch maps a request onto the supervisor's public API.
func (s *Server) dispatch(ctx context.Context, req Request) Response {
	switch req.Command {
	case CmdPing:
		return Response{OK: true, Message: "pong"}

	case CmdList:
		services, err := s.sup.List(ctx)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Services: services}

	case CmdStatus:
		st, err := s.sup.Status(ctx, req.Service)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Status: &st}

	case CmdStart:
		if err := s.sup.StartService(ctx, req.Service); err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("started %s", req.Service)}

	case CmdStop:
		if err := s.sup.StopService(ctx, req.Service); err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("stopped %s", req.Service)}

	case CmdRestart:
		if err := s.sup.RestartService(ctx, req.Service); err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("restarted %s", req.Service)}

	case CmdApply:
		services, err := s.sup.ApplyNow(ctx)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Services: services, Message: "applied"}

	case CmdEvents:
		events, err := s.sup.Events(ctx, req.Service, req.Limit)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Events: events}

	case CmdBackup:
		// An empty service name backs up everything (§10).
		if req.Service == "" {
			archives, err := s.sup.BackupAll(ctx)
			if err != nil {
				return errorResponse(err)
			}
			return Response{OK: true, Archives: archives, Message: fmt.Sprintf("backed up %d service(s)", len(archives))}
		}
		archive, err := s.sup.Backup(ctx, req.Service)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Archives: []backup.Archive{archive}, Message: "backed up " + req.Service}

	case CmdRestore:
		archive, err := s.sup.Restore(ctx, req.Service, req.Timestamp)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Archives: []backup.Archive{archive},
			Message: fmt.Sprintf("restored %s from %s", req.Service, archive.Timestamp)}

	case CmdBackups:
		archives, err := s.sup.ListBackups(ctx, req.Service)
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Archives: archives}

	default:
		return errorResponse(fmt.Errorf("unknown command %q", req.Command))
	}
}

func writeResponse(w io.Writer, resp Response) {
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

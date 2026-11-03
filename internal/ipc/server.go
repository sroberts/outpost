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
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/secrets"
	"github.com/sroberts/outpost/internal/supervisor"
	"github.com/sroberts/outpost/internal/version"
)

// maxSocketPath is the shortest of the platform sockaddr_un limits, so the
// check behaves the same on darwin and linux.
const maxSocketPath = 104

// Server exposes a Supervisor over a Unix socket.
type Server struct {
	sup     *supervisor.Supervisor
	auth    *auth.Store
	secrets *secrets.Store
	gateway GatewayStatus
	config  string
	started time.Time
	path    string
	ln      net.Listener
	logf    func(string, ...any)

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

	return &Server{sup: sup, started: time.Now(), path: path, ln: ln, logf: logf}, nil
}

// UseAuth enables the user.* and auth.* commands. Call it before Serve. It's
// a setter rather than a Listen parameter so a daemon, or a test, that has no
// auth store keeps working unchanged.
func (s *Server) UseAuth(a *auth.Store) { s.auth = a }

// GatewayStatus is the part of the gateway the socket reports on.
type GatewayStatus interface {
	Status() gateway.Status
}

// UseGateway enables gateway.status. Call it before Serve; a daemon with the
// gateway off doesn't.
func (s *Server) UseGateway(g GatewayStatus) { s.gateway = g }

// UseConfig records the fingerprint of the config the daemon started with,
// for ping to report. Call it before Serve.
func (s *Server) UseConfig(fingerprint string) { s.config = fingerprint }

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
		return Response{OK: true, Message: "pong", Version: version.Short(), Config: s.config, PID: os.Getpid(), Started: s.started}

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

	case CmdForget:
		if err := s.sup.Forget(ctx, req.Service); err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("cleared %s", req.Service)}

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

	case CmdUserAdd, CmdUserPasswd, CmdUserRemove, CmdUserList, CmdUserGroups,
		CmdUserDisable, CmdUserEnable, CmdUserLogout, CmdUserAdmin, CmdUserSignIns, CmdUserTailnet, CmdAuthRotate:
		return s.dispatchAuth(ctx, req)

	case CmdTokenCreate, CmdTokenList, CmdTokenRevoke:
		return s.dispatchToken(ctx, req)

	case CmdSecretList, CmdSecretSet, CmdSecretRemove, CmdSecretBind, CmdSecretUnbind:
		return s.dispatchSecret(ctx, req)

	case CmdGatewayStatus:
		if s.gateway == nil {
			return errorResponse(errors.New("the gateway is off; turn it on with [gateway] enabled = true in config.toml, then run `outpost daemon restart`"))
		}
		st := s.gateway.Status()
		return Response{OK: true, Gateway: &st}

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

// errNoAuth is what user management says on a daemon without an auth store,
// instead of a nil-pointer panic.
var errNoAuth = errors.New("user management is not available: this daemon was started without an auth store")

// dispatchAuth serves the user.* and auth.* commands. They go straight to the
// auth store rather than through the supervisor's loop: auth.db is its own
// database with its own locking, and nothing here touches supervisor state.
func (s *Server) dispatchAuth(ctx context.Context, req Request) Response {
	if s.auth == nil {
		return errorResponse(errNoAuth)
	}
	if req.Command == CmdAuthRotate {
		k, err := s.auth.RotateKey()
		if err != nil {
			return errorResponse(err)
		}
		return Response{OK: true, Message: fmt.Sprintf("new signing key %s; the old one stays published for %.0fh",
			k.ID, auth.KeyGracePeriod.Hours())}
	}
	if req.Command == CmdUserList {
		users, err := s.auth.ListUsers()
		if err != nil {
			return errorResponse(err)
		}
		infos := make([]UserInfo, 0, len(users))
		for _, u := range users {
			infos = append(infos, userInfo(u))
		}
		return Response{OK: true, Users: infos}
	}

	if req.User == nil || req.User.Username == "" {
		return errorResponse(fmt.Errorf("%s: a username is required", req.Command))
	}
	u := *req.User
	switch req.Command {
	case CmdUserAdd:
		err := s.auth.AddUser(auth.User{
			Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
			Admin: u.Admin, Groups: u.AddGroups,
		}, u.Password)
		if err != nil {
			return errorResponse(err)
		}
		msg := "added user " + u.Username
		if u.Admin {
			msg += " (admin)"
		}
		if u.Password == "" {
			msg += "; they can't log in until `outpost user passwd " + u.Username + "` sets a password"
		}
		return Response{OK: true, Message: msg}

	case CmdUserPasswd:
		tokens := s.liveTokens(u.Username)
		if err := s.auth.SetPassword(u.Username, u.Password); err != nil {
			return errorResponse(err)
		}
		s.tokensRevoked(ctx, tokens, u.Username, "password set")
		return Response{OK: true, Message: "set the password for " + u.Username + "; their existing sessions have ended and any access tokens are revoked"}

	case CmdUserRemove:
		tokens := s.liveTokens(u.Username)
		if err := s.auth.RemoveUser(u.Username); err != nil {
			return errorResponse(err)
		}
		s.tokensRevoked(ctx, tokens, u.Username, "user removed")
		return Response{OK: true, Message: "removed user " + u.Username}

	case CmdUserGroups:
		groups, err := s.auth.UpdateGroups(u.Username, u.AddGroups, u.RemoveGroups)
		if err != nil {
			return errorResponse(err)
		}
		list := "no groups"
		if len(groups) > 0 {
			list = strings.Join(groups, ", ")
		}
		return Response{OK: true, Message: u.Username + ": " + list}

	case CmdUserDisable, CmdUserEnable:
		disable := req.Command == CmdUserDisable
		tokens := 0
		if disable {
			tokens = s.liveTokens(u.Username)
		}
		if err := s.auth.SetDisabled(u.Username, disable); err != nil {
			return errorResponse(err)
		}
		if disable {
			s.tokensRevoked(ctx, tokens, u.Username, "user disabled")
			return Response{OK: true, Message: "disabled " + u.Username + ", ended their sessions and revoked any access tokens"}
		}
		return Response{OK: true, Message: "enabled " + u.Username}

	case CmdUserAdmin:
		if err := s.auth.SetAdmin(u.Username, u.Admin); err != nil {
			return errorResponse(err)
		}
		if u.Admin {
			return Response{OK: true, Message: u.Username + " is now an admin"}
		}
		return Response{OK: true, Message: u.Username + " is no longer an admin"}

	case CmdUserTailnet:
		if err := s.auth.SetTailnetLogin(u.Username, u.TailnetLogin); err != nil {
			return errorResponse(err)
		}
		if u.TailnetLogin == "" {
			return Response{OK: true, Message: u.Username + " has no tailnet login linked; a login equal to their email still counts as them"}
		}
		return Response{OK: true, Message: "linked tailnet login " + auth.NormalizeTailnetLogin(u.TailnetLogin) + " to " + u.Username}

	case CmdUserSignIns:
		list, err := s.auth.SignIns(u.Username)
		if err != nil {
			return errorResponse(err)
		}
		infos := make([]SignInInfo, 0, len(list))
		for _, si := range list {
			infos = append(infos, SignInInfo{UserAgent: si.UserAgent, IP: si.IP, CreatedAt: si.CreatedAt, LastSeen: si.LastSeen, Apps: si.Apps})
		}
		return Response{OK: true, SignIns: infos}

	case CmdUserLogout:
		n, tokens, err := s.auth.RevokeUserAccess(u.Username)
		if err != nil {
			return errorResponse(err)
		}
		s.tokensRevoked(ctx, tokens, u.Username, "user logged out")
		return Response{OK: true, Message: fmt.Sprintf("ended %d session(s) and revoked %d access token(s) for %s", n, tokens, u.Username)}
	}
	return errorResponse(fmt.Errorf("unknown command %q", req.Command))
}

func writeResponse(w io.Writer, resp Response) {
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return
	}
}

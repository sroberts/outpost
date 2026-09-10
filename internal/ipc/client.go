package ipc

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/supervisor"
)

// ErrNoDaemon means nothing is listening on the socket.
var ErrNoDaemon = errors.New("no outpost daemon is running")

// Client talks to the daemon over its Unix socket.
type Client struct {
	path    string
	timeout time.Duration
}

// NewClient returns a client for the socket at path.
func NewClient(path string) *Client {
	return &Client{path: path, timeout: requestTimeout}
}

// Available reports whether a daemon is accepting connections.
func (c *Client) Available() bool {
	if _, err := os.Stat(c.path); err != nil {
		return false
	}
	conn, err := net.DialTimeout("unix", c.path, time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// do sends one request and returns the daemon's response.
func (c *Client) do(req Request) (Response, error) {
	conn, err := net.DialTimeout("unix", c.path, 2*time.Second)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || isConnRefused(err) {
			return Response{}, ErrNoDaemon
		}
		return Response{}, fmt.Errorf("connect to %s: %w", c.path, err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(c.timeout))

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	if _, err := conn.Write(body); err != nil {
		return Response{}, fmt.Errorf("send request: %w", err)
	}
	// Signal end-of-request so the daemon's ReadAll returns.
	if uc, ok := conn.(*net.UnixConn); ok {
		_ = uc.CloseWrite()
	}

	var resp Response
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		if err == io.EOF {
			return Response{}, fmt.Errorf("daemon closed the connection without replying")
		}
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if !resp.OK {
		return resp, errors.New(resp.Error)
	}
	return resp, nil
}

// List returns every service the daemon knows about.
func (c *Client) List() ([]supervisor.ServiceStatus, error) {
	resp, err := c.do(Request{Command: CmdList})
	return resp.Services, err
}

// Status returns one service's detail.
func (c *Client) Status(name string) (supervisor.ServiceStatus, error) {
	resp, err := c.do(Request{Command: CmdStatus, Service: name})
	if err != nil {
		return supervisor.ServiceStatus{}, err
	}
	if resp.Status == nil {
		return supervisor.ServiceStatus{}, fmt.Errorf("daemon returned no status for %q", name)
	}
	return *resp.Status, nil
}

// Start, Stop and Restart act on one service.
func (c *Client) Start(name string) (string, error) {
	resp, err := c.do(Request{Command: CmdStart, Service: name})
	return resp.Message, err
}

func (c *Client) Stop(name string) (string, error) {
	resp, err := c.do(Request{Command: CmdStop, Service: name})
	return resp.Message, err
}

func (c *Client) Restart(name string) (string, error) {
	resp, err := c.do(Request{Command: CmdRestart, Service: name})
	return resp.Message, err
}

// Apply reloads manifests and reconciles.
func (c *Client) Apply() ([]supervisor.ServiceStatus, error) {
	resp, err := c.do(Request{Command: CmdApply})
	return resp.Services, err
}

// Events returns recent activity, newest first.
func (c *Client) Events(service string, limit int) ([]supervisor.EventRecord, error) {
	resp, err := c.do(Request{Command: CmdEvents, Service: service, Limit: limit})
	return resp.Events, err
}

// Backup archives one service, or every service when name is empty.
func (c *Client) Backup(name string) ([]backup.Archive, string, error) {
	resp, err := c.do(Request{Command: CmdBackup, Service: name})
	return resp.Archives, resp.Message, err
}

// Restore replaces a service's storage from an archive. An empty timestamp
// selects the most recent one.
func (c *Client) Restore(name, timestamp string) (string, error) {
	resp, err := c.do(Request{Command: CmdRestore, Service: name, Timestamp: timestamp})
	return resp.Message, err
}

// Backups lists a service's archives, newest first.
func (c *Client) Backups(name string) ([]backup.Archive, error) {
	resp, err := c.do(Request{Command: CmdBackups, Service: name})
	return resp.Archives, err
}

// isConnRefused reports whether dialing failed because nothing is listening,
// which is a stale socket rather than a real error worth surfacing.
//
// net.DialTimeout wraps every failure in *net.OpError, so matching on that type
// would swallow genuine errors — an over-long socket path or a non-socket file
// would both be reported as "no daemon is running". Match the specific errno.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, os.ErrNotExist)
}

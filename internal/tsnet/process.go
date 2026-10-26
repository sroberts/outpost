package tsnet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http/httputil"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Each node runs in a child process rather than inside the daemon.
//
// Tailscale's ipnlocal watchdog panics, unconditionally and from a timer
// goroutine, when LocalBackend stalls for over half a minute (#31). A panic
// on a goroutine the daemon did not start cannot be recovered, so in-process
// it took down the daemon and every supervised service with it, for a reason
// that had nothing to do with any of them. In a child it costs one service its
// tailnet exposure until the router restarts the node.

// NodeCommand is the hidden subcommand that runs one node. The daemon
// re-executes its own binary with it, so there is nothing extra to install.
const NodeCommand = "__tsnet-node"

// ReadyLine is what a node prints on stdout once it is serving on the
// tailnet. Everything it prints on stderr is operator-facing log output,
// including the one-time login URL.
const ReadyLine = "outpost-tsnet-node: ready"

// AuthKeyEnv carries the auth key to the node. An argument would put the key
// in the process table for any local user to read.
const AuthKeyEnv = "OUTPOST_TSNET_AUTHKEY"

// defaultNodeStopGrace bounds how long a node gets to leave the tailnet
// cleanly once asked to stop.
const defaultNodeStopGrace = 10 * time.Second

// NewProxy is the reverse proxy a node serves, exported for the node process.
func NewProxy(port int) *httputil.ReverseProxy { return newProxy(port) }

// nodeArgs are the arguments, after NodeCommand, that start a node for cfg.
func nodeArgs(cfg nodeConfig) []string {
	args := []string{
		"--service", cfg.Service,
		"--hostname", cfg.Hostname,
		"--dir", cfg.Dir,
		"--port", strconv.Itoa(cfg.Port),
	}
	if cfg.Verbose {
		args = append(args, "--verbose")
	}
	return args
}

// defaultNodeCmd re-executes the running binary as a node.
func defaultNodeCmd(cfg nodeConfig) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the outpost binary: %w", err)
	}
	return exec.Command(self, append([]string{NodeCommand}, nodeArgs(cfg)...)...), nil
}

// processNode is a node running in a child process. It serves the tailnet
// itself, so it has no listener for the router to serve.
type processNode struct {
	cmd   *exec.Cmd
	stdin io.Closer
	grace time.Duration

	done    chan struct{}
	exitErr error // set before done closes

	closeOnce sync.Once
}

func (p *processNode) Listener() net.Listener { return nil }

// Done closes when the process has exited, for any reason.
func (p *processNode) Done() <-chan struct{} { return p.done }

// Close asks the node to leave by closing its stdin, and kills it if it has
// not gone within the grace period.
func (p *processNode) Close() error {
	p.closeOnce.Do(func() {
		// The node exits on EOF; a failed close means the pipe is already
		// gone, which it notices the same way.
		_ = p.stdin.Close()
	})
	select {
	case <-p.done:
	case <-time.After(p.grace):
		// It did not leave in time; it is being torn down regardless, and
		// Kill only fails on a process that has already exited.
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// startProcessNode is the real backend: a child process running one
// tsnet.Server. It returns once the node reports it is serving, which on a
// first run without an auth key means once someone has visited the login URL.
func (r *Router) startProcessNode(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("create tsnet state dir: %w", err)
	}
	cmd, err := r.nodeCmd(cfg)
	if err != nil {
		return nil, err
	}
	// Added to, not replaced: whatever environment nodeCmd built is kept.
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, AuthKeyEnv+"="+cfg.AuthKey)
	// Its own process group, so a Ctrl-C meant for a foreground daemon does
	// not reach the node first and look like a crash; the daemon stops it in
	// order on the way out.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// stdin is never written. It exists so the node sees EOF when the daemon
	// goes away for any reason, SIGKILL included, and exits rather than
	// lingering with the state directory held.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start tsnet node %q: %w", cfg.Hostname, err)
	}

	p := &processNode{cmd: cmd, stdin: stdin, grace: r.nodeStopGrace, done: make(chan struct{})}
	ready := make(chan struct{})

	// The last few stderr lines explain an exit before ready far better than
	// an exit status does.
	var tailMu sync.Mutex
	var tail []string

	var streams sync.WaitGroup
	streams.Add(2)
	go func() {
		defer streams.Done()
		var once sync.Once
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if sc.Text() == ReadyLine {
				once.Do(func() { close(ready) })
				continue
			}
			r.logf("%s: %s", cfg.Service, sc.Text())
		}
		drain(stdout)
	}()
	go func() {
		defer streams.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			r.logf("%s: %s", cfg.Service, line)
			tailMu.Lock()
			if tail = append(tail, line); len(tail) > 3 {
				tail = tail[1:]
			}
			tailMu.Unlock()
		}
		drain(stderr)
	}()
	go func() {
		// Wait must follow the last read from the pipes, or it can close
		// them under the scanners and lose the output that explains an exit.
		streams.Wait()
		p.exitErr = cmd.Wait()
		close(p.done)
	}()

	select {
	case <-ready:
		return p, nil
	case <-p.done:
		tailMu.Lock()
		why := strings.Join(tail, "; ")
		tailMu.Unlock()
		if why == "" {
			why = fmt.Sprint(p.exitErr)
		}
		return nil, fmt.Errorf("tsnet node %q exited before coming up: %s", cfg.Hostname, why)
	case <-ctx.Done():
		// Detached while waiting, typically on a login nobody has done yet.
		_ = p.Close() // never fails; it kills what does not leave
		return nil, ctx.Err()
	}
}

// drain discards what a scanner gave up on -- a line over its limit -- so the
// node never blocks writing to a pipe nobody reads.
func drain(r io.Reader) { _, _ = io.Copy(io.Discard, r) }

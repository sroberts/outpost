// Package node runs one Tailscale node: the body of the hidden subcommand the
// daemon starts for each service on the tailnet (see internal/tsnet).
//
// It lives apart from the router so that the daemon process never links a
// tsnet.Server into its own goroutines; a panic in here ends this process
// and nothing else.
package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"tailscale.com/tsnet"

	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// Config is one node.
type Config struct {
	Service  string
	Hostname string
	Dir      string
	AuthKey  string
	Port     int
	Verbose  bool
}

// Run brings the node up, prints outposttsnet.ReadyLine to ready once it is
// serving, and proxies tailnet traffic to the service until ctx ends. Log
// lines, including the one-time login URL, go to logs.
func Run(ctx context.Context, cfg Config, ready, logs io.Writer) error {
	logf := func(format string, args ...any) { fmt.Fprintf(logs, format+"\n", args...) }
	quiet := func(string, ...any) {}

	server := &tsnet.Server{
		Dir:       cfg.Dir,
		Hostname:  cfg.Hostname,
		AuthKey:   cfg.AuthKey,
		Ephemeral: false,
		// tsnet's own logging is noisy; only the login URL and other
		// user-facing lines come through unless verbose was asked for.
		Logf:     quiet,
		UserLogf: logf,
	}
	if cfg.Verbose {
		server.Logf = logf
	}
	// The process is exiting either way; a failure to close is not actionable.
	defer func() { _ = server.Close() }()

	if err := server.Start(); err != nil {
		return fmt.Errorf("start tsnet node %q: %w", cfg.Hostname, err)
	}
	// Block until the node is actually usable, so a node that never registers
	// is retried by the daemon rather than reported as attached.
	if _, err := server.Up(ctx); err != nil {
		return fmt.Errorf("tsnet node %q did not come up: %w", cfg.Hostname, err)
	}
	ln, err := server.Listen("tcp", ":80")
	if err != nil {
		return fmt.Errorf("listen on tailnet: %w", err)
	}

	srv := &http.Server{
		Handler:           outposttsnet.NewProxy(cfg.Port),
		ReadHeaderTimeout: 30 * time.Second,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	if _, err := fmt.Fprintln(ready, outposttsnet.ReadyLine); err != nil {
		return fmt.Errorf("report ready: %w", err)
	}

	select {
	case err := <-served:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("tailnet listener stopped: %w", err)
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Leaving anyway; in-flight requests that do not finish are cut off.
		_ = srv.Shutdown(shutdown)
		return nil
	}
}

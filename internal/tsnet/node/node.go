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
	"net"
	"net/http"
	"net/url"
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
	// Upstream, when set, is the login gateway's URL. The node then serves
	// HTTPS and sends everything there rather than to the service, so the
	// tailnet address is behind the same login as the portal.
	Upstream           string
	UpstreamServerName string
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
	status, err := server.Up(ctx)
	if err != nil {
		return fmt.Errorf("tsnet node %q did not come up: %w", cfg.Hostname, err)
	}
	if status.Self != nil && status.Self.DNSName != "" {
		if _, err := fmt.Fprintln(ready, outposttsnet.HostLinePrefix+status.Self.DNSName); err != nil {
			return fmt.Errorf("report host: %w", err)
		}
	}

	var servers []*http.Server
	served := make(chan error, 2)
	serve := func(ln net.Listener, h http.Handler) {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
		servers = append(servers, srv)
		go func() { served <- srv.Serve(ln) }()
	}

	if cfg.Upstream == "" {
		ln, err := server.Listen("tcp", ":80")
		if err != nil {
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		serve(ln, outposttsnet.NewProxy(cfg.Port))
	} else {
		target, err := url.Parse(cfg.Upstream)
		if err != nil {
			return fmt.Errorf("upstream %q: %w", cfg.Upstream, err)
		}
		// The gateway's session cookie is Secure, so the app has to be
		// served over HTTPS; the certificate is the tailnet's own.
		tlsLn, err := server.ListenTLS("tcp", ":443")
		if err != nil {
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		ln, err := server.Listen("tcp", ":80")
		if err != nil {
			_ = tlsLn.Close() // failing anyway
			return fmt.Errorf("listen on tailnet: %w", err)
		}
		serve(tlsLn, outposttsnet.NewUpstreamProxy(target, cfg.UpstreamServerName))
		serve(ln, http.HandlerFunc(toHTTPS))
	}

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
		for _, srv := range servers {
			// Leaving anyway; in-flight requests that do not finish are cut off.
			_ = srv.Shutdown(shutdown)
		}
		return nil
	}
}

// toHTTPS sends a plain-http request to the same place over HTTPS.
func toHTTPS(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Scheme = "https"
	u.Host = r.Host
	http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
}

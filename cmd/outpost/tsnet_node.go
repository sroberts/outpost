package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/sroberts/outpost/internal/manifest"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
	"github.com/sroberts/outpost/internal/tsnet/node"
)

// runTsnetNode is the hidden subcommand the daemon starts for each service on
// the tailnet. It is not for operators: the daemon supplies every argument.
func runTsnetNode(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet(outposttsnet.NodeCommand, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg node.Config
	fs.StringVar(&cfg.Service, "service", "", "service name")
	fs.StringVar(&cfg.Hostname, "hostname", "", "tailnet hostname")
	fs.StringVar(&cfg.Dir, "dir", "", "node state directory")
	fs.IntVar(&cfg.Port, "port", 0, "service port on loopback")
	fs.BoolVar(&cfg.Verbose, "verbose", false, "pass tsnet's own logging through")
	fs.StringVar(&cfg.Upstream, "upstream", "", "send traffic here, the login gateway, instead of the service")
	fs.StringVar(&cfg.UpstreamServerName, "upstream-server-name", "", "verify an https upstream's certificate for this name")
	fs.BoolVar(&cfg.Funnel, "funnel", false, "also answer the internet through Tailscale Funnel, sending it to --public-upstream")
	fs.StringVar(&cfg.PublicUpstream, "public-upstream", "", "the gateway's public listener, for what arrives through Funnel")
	fs.Func("tcp", "forward a tailnet port to the service, as <tailnet port>=<local port>; repeatable", func(v string) error {
		f, err := manifest.ParseTCPForward(v)
		if err != nil {
			return err
		}
		cfg.TCP = append(cfg.TCP, f)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.Service == "" || cfg.Hostname == "" || cfg.Dir == "" {
		return fmt.Errorf("%s: --service, --hostname, --dir and --port are required", outposttsnet.NodeCommand)
	}
	// In front of the gateway the node never dials the service, and the
	// portal's node has no service at all.
	if cfg.Port == 0 && cfg.Upstream == "" {
		return fmt.Errorf("%s: --port is required without --upstream", outposttsnet.NodeCommand)
	}
	if cfg.Funnel && (cfg.Upstream == "" || cfg.PublicUpstream == "") {
		return fmt.Errorf("%s: --funnel needs --upstream and --public-upstream", outposttsnet.NodeCommand)
	}
	cfg.AuthKey = os.Getenv(outposttsnet.AuthKeyEnv)
	cfg.AccessSecret = os.Getenv(outposttsnet.AccessSecretEnv)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// stdin is a pipe from the daemon that is never written. EOF means the
	// daemon closed it or died, and either way this node should leave rather
	// than linger holding its state directory.
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		cancel()
	}()

	return node.Run(ctx, cfg, stdout, stderr)
}

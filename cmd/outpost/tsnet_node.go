package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.Service == "" || cfg.Hostname == "" || cfg.Dir == "" || cfg.Port == 0 {
		return fmt.Errorf("%s: --service, --hostname, --dir and --port are required", outposttsnet.NodeCommand)
	}
	cfg.AuthKey = os.Getenv(outposttsnet.AuthKeyEnv)

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

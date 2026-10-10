package main

import (
	"bytes"
	"strings"
	"testing"

	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// The daemon supplies every argument. A node started without them must say
// so and exit, not come up as an unnamed tailnet device.
func TestTsnetNode_RequiresItsArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := run([]string{outposttsnet.NodeCommand, "--service", "svc"}, &stdout, &stderr)
	if err == nil {
		t.Fatal("a node with no hostname, dir or port was started")
	}
	if !strings.Contains(err.Error(), "--hostname") {
		t.Errorf("error = %v; want it to name the missing arguments", err)
	}
}

// The node subcommand is internal; it must not be offered to operators.
func TestTsnetNode_IsNotInTheUsage(t *testing.T) {
	if strings.Contains(usage, outposttsnet.NodeCommand) {
		t.Errorf("usage advertises %s", outposttsnet.NodeCommand)
	}
}

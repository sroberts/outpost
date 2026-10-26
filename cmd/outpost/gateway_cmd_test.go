package main

import (
	"strings"
	"testing"
)

func TestGatewayCmd(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "gateway"); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("bare gateway: %v", err)
	}
	if _, _, err := runCLI(t, home, "gateway", "status"); err == nil {
		t.Error("gateway status with no daemon: want an error")
	}
}

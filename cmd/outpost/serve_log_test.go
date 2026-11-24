package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/config"
)

// --log-level beats OUTPOST_LOG_LEVEL, which beats [log] level, and a value
// that isn't a level is refused, naming where it came from.
func TestDaemonLogLevel_Precedence(t *testing.T) {
	a := &app{home: t.TempDir(), config: config.Config{Log: config.Log{Level: "warn"}}}

	t.Setenv("OUTPOST_LOG_LEVEL", "")
	if got, err := a.daemonLogLevel(""); err != nil || got != "warn" {
		t.Errorf("from config.toml: %q, %v; want warn", got, err)
	}
	t.Setenv("OUTPOST_LOG_LEVEL", "ERROR")
	if got, err := a.daemonLogLevel(""); err != nil || got != "error" {
		t.Errorf("from the environment: %q, %v; want error", got, err)
	}
	if got, err := a.daemonLogLevel("debug"); err != nil || got != "debug" {
		t.Errorf("from the flag: %q, %v; want debug", got, err)
	}

	t.Setenv("OUTPOST_LOG_LEVEL", "chatty")
	if _, err := a.daemonLogLevel(""); err == nil || !strings.Contains(err.Error(), "OUTPOST_LOG_LEVEL") {
		t.Errorf("a bad environment level: %v", err)
	}
	if _, err := a.daemonLogLevel("loud"); err == nil || !strings.Contains(err.Error(), "--log-level") {
		t.Errorf("a bad flag: %v", err)
	}
}

func TestDaemonLogPath(t *testing.T) {
	a := &app{home: "/h"}
	if got := a.daemonLogPath(); got != filepath.Join("/h", "logs", "outpost.log") {
		t.Errorf("daemonLogPath = %q", got)
	}
}

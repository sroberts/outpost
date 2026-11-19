package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLog_DefaultsAndValidation(t *testing.T) {
	var l Log
	if l.LevelName() != "info" || l.FormatName() != "text" {
		t.Errorf("defaults = %s/%s; want info/text", l.LevelName(), l.FormatName())
	}
	for _, bad := range []Log{{Level: "loud"}, {Format: "xml"}} {
		if err := (Config{Log: bad}).Validate(); err == nil || !strings.Contains(err.Error(), "log:") {
			t.Errorf("Validate(%+v) = %v; want a log: error", bad, err)
		}
	}
	if err := (Config{Log: Log{Level: "WARN", Format: "JSON"}}).Validate(); err != nil {
		t.Errorf("upper case refused: %v", err)
	}
}

func TestLog_LoadsAndTakesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[log]\nlevel = \"debug\"\nformat = \"json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.LevelName() != "debug" || cfg.Log.FormatName() != "json" {
		t.Errorf("loaded %+v", cfg.Log)
	}
	// A daemon running with other settings is stale; the defaults spelled
	// out are not a change.
	base := Defaults()
	if cfg.Fingerprint() == base.Fingerprint() {
		t.Error("changing [log] didn't change the fingerprint")
	}
	spelled := Defaults()
	spelled.Log = Log{Level: "info", Format: "text"}
	if spelled.Fingerprint() != base.Fingerprint() {
		t.Error("[log] with the defaults spelled out changed the fingerprint")
	}
}

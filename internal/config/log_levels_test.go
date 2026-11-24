package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogLevels_LoadAndApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[log]\nlevel = \"warn\"\n\n[log.levels]\ngateway = \"DEBUG\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Log.LevelFor("gateway", cfg.Log.LevelName()); got != "debug" {
		t.Errorf("gateway logs at %q; want debug", got)
	}
	if got := cfg.Log.LevelFor("supervisor", cfg.Log.LevelName()); got != "warn" {
		t.Errorf("supervisor logs at %q; want [log] level's warn", got)
	}
	// An override of the base level leaves a part's own level alone.
	if got := cfg.Log.LevelFor("gateway", "error"); got != "debug" {
		t.Errorf("gateway logs at %q after an override; want debug", got)
	}
}

func TestLogLevels_Validation(t *testing.T) {
	for _, tc := range []struct {
		levels map[string]string
		want   string
	}{
		{map[string]string{"gatway": "debug"}, `"gatway" is not a part of the daemon`},
		{map[string]string{"gateway": "loud"}, `gateway = "loud"`},
	} {
		err := (Config{Log: Log{Levels: tc.levels}}).Validate()
		if err == nil || !strings.Contains(err.Error(), "log: levels:") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%v) = %v; want a log: levels: error saying %s", tc.levels, err, tc.want)
		}
	}
	every := map[string]string{}
	for _, c := range LogComponents {
		every[c] = "error"
	}
	if err := (Config{Log: Log{Levels: every}}).Validate(); err != nil {
		t.Errorf("every part refused: %v", err)
	}
}

func TestLogLevels_ChangeTheFingerprint(t *testing.T) {
	base := Defaults()
	turned := Defaults()
	turned.Log.Levels = map[string]string{"gateway": "debug"}
	if turned.Fingerprint() == base.Fingerprint() {
		t.Error("[log.levels] didn't change the fingerprint")
	}
	empty := Defaults()
	empty.Log.Levels = map[string]string{}
	if empty.Fingerprint() != base.Fingerprint() {
		t.Error("an empty [log.levels] changed the fingerprint")
	}
}

func TestValidLogLevel(t *testing.T) {
	for _, ok := range []string{"debug", "INFO", " warn ", "error"} {
		if err := ValidLogLevel(ok); err != nil {
			t.Errorf("ValidLogLevel(%q) = %v", ok, err)
		}
	}
	if err := ValidLogLevel("verbose"); err == nil {
		t.Error("verbose accepted")
	}
}

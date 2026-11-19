package config

import (
	"fmt"
	"strings"
)

// Log is how the daemon writes its own log: the lines launchd puts in
// logs/outpost.out.log and systemd puts in the journal. It doesn't change how
// services' output is captured.
type Log struct {
	// Level is the least severe line written: debug, info, warn or error.
	// Unset means info.
	Level string `toml:"level"`
	// Format is text (the default: a timestamp, the level and the message,
	// coloured on a terminal), json or logfmt, for a collector to parse.
	Format string `toml:"format"`
}

// LogLevels and LogFormats are the values [log] accepts.
var (
	LogLevels  = []string{"debug", "info", "warn", "error"}
	LogFormats = []string{"text", "json", "logfmt"}
)

// LevelName is the level to write at, with the default filled in.
func (l Log) LevelName() string {
	if l.Level == "" {
		return "info"
	}
	return strings.ToLower(l.Level)
}

// FormatName is the format to write in, with the default filled in.
func (l Log) FormatName() string {
	if l.Format == "" {
		return "text"
	}
	return strings.ToLower(l.Format)
}

func (l Log) validate() error {
	if !oneOf(l.LevelName(), LogLevels) {
		return fmt.Errorf("level %q: use one of %s", l.Level, strings.Join(LogLevels, ", "))
	}
	if !oneOf(l.FormatName(), LogFormats) {
		return fmt.Errorf("format %q: use one of %s", l.Format, strings.Join(LogFormats, ", "))
	}
	return nil
}

func oneOf(s string, set []string) bool {
	for _, v := range set {
		if s == v {
			return true
		}
	}
	return false
}

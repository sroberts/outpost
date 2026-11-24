package config

import (
	"fmt"
	"sort"
	"strings"
)

// Log is how the daemon writes its own log: the lines launchd puts in
// logs/outpost.out.log and systemd puts in the journal, and the daemon itself
// keeps in logs/outpost.log. It doesn't change how services' output is
// captured.
type Log struct {
	// Level is the least severe line written: debug, info, warn or error.
	// Unset means info.
	Level string `toml:"level"`
	// Format is text (the default: a timestamp, the level and the message,
	// coloured on a terminal), json or logfmt, for a collector to parse.
	Format string `toml:"format"`
	// Levels sets the level for one part of the daemon, by the name its
	// lines carry ([log.levels] gateway = "debug"), so one area can be turned
	// up, or down, without the rest. A part left out logs at Level.
	Levels map[string]string `toml:"levels"`
}

// LogLevels and LogFormats are the values [log] accepts, and LogComponents
// the names [log.levels] does: the prefix each part of the daemon logs with.
var (
	LogLevels     = []string{"debug", "info", "warn", "error"}
	LogFormats    = []string{"text", "json", "logfmt"}
	LogComponents = []string{"supervisor", "gateway", "ipc", "ai", "mail", "tsnet", "stats"}
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

// LevelFor is the level component logs at: its own from [log.levels] if it
// has one, otherwise base, which is Level or what overrides it for this run.
// The more specific setting wins, so turning one part down survives a
// --log-level debug aimed at the rest.
func (l Log) LevelFor(component, base string) string {
	if v := l.Levels[component]; v != "" {
		return strings.ToLower(v)
	}
	return base
}

// levelsKey is [log.levels] in a stable order, for Fingerprint.
func (l Log) levelsKey() string {
	parts := make([]string, 0, len(l.Levels))
	for k, v := range l.Levels {
		parts = append(parts, k+"="+strings.ToLower(v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (l Log) validate() error {
	if !oneOf(l.LevelName(), LogLevels) {
		return fmt.Errorf("level %q: use one of %s", l.Level, strings.Join(LogLevels, ", "))
	}
	if !oneOf(l.FormatName(), LogFormats) {
		return fmt.Errorf("format %q: use one of %s", l.Format, strings.Join(LogFormats, ", "))
	}
	components := make([]string, 0, len(l.Levels))
	for c := range l.Levels {
		components = append(components, c)
	}
	sort.Strings(components) // so the same file always names the same mistake
	for _, c := range components {
		if !oneOf(c, LogComponents) {
			return fmt.Errorf("levels: %q is not a part of the daemon; use one of %s", c, strings.Join(LogComponents, ", "))
		}
		if !oneOf(strings.ToLower(l.Levels[c]), LogLevels) {
			return fmt.Errorf("levels: %s = %q: use one of %s", c, l.Levels[c], strings.Join(LogLevels, ", "))
		}
	}
	return nil
}

// ValidLogLevel refuses a level [log] wouldn't accept, for the overrides that
// don't come through the file: --log-level and OUTPOST_LOG_LEVEL.
func ValidLogLevel(level string) error {
	if !oneOf(strings.ToLower(strings.TrimSpace(level)), LogLevels) {
		return fmt.Errorf("log level %q: use one of %s", level, strings.Join(LogLevels, ", "))
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

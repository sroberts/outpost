// Package version identifies the running build.
//
// A daemon installed by `outpost install` can run unattended for months. When
// it misbehaves the first question is which binary is actually running, and
// answering it should not require guessing from file timestamps.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set at link time by the release build:
//
//	go build -ldflags "-X github.com/sroberts/outpost/internal/version.Version=v1.2.3"
//
// Left empty by a plain `go build`, which falls back to the VCS stamp the Go
// toolchain embeds on its own — so even an unreleased local build identifies
// itself rather than claiming to be nothing in particular.
var (
	Version string
	Commit  string
	Date    string
)

// Info is what the binary knows about itself.
type Info struct {
	Version string
	Commit  string
	Date    string
	// Dirty reports that the working tree had uncommitted changes at build
	// time, which makes Commit an approximation rather than an identity.
	Dirty bool
	// DateIsCommit distinguishes the two things Date can be. With no link-time
	// stamp the only timestamp available is the commit's, and calling that a
	// build date would mislead whoever is reading it during an incident.
	DateIsCommit bool
	Go           string
	OS           string
	Arch         string
}

// Get reads the build stamp, preferring link-time values and falling back to
// what the toolchain recorded.
func Get() Info {
	i := Info{
		Version: Version,
		Commit:  Commit,
		Date:    Date,
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if i.Commit == "" {
					i.Commit = s.Value
				}
			case "vcs.time":
				if i.Date == "" {
					i.Date = s.Value
					i.DateIsCommit = true
				}
			case "vcs.modified":
				i.Dirty = s.Value == "true"
			}
		}
		if i.Version == "" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			i.Version = bi.Main.Version
		}
	}

	if i.Version == "" {
		i.Version = "dev"
	}
	return i
}

// short is the commit trimmed to the usual display length.
func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// String is the one-line form, for logs and the daemon banner.
func (i Info) String() string {
	var b strings.Builder
	b.WriteString(i.Version)
	if i.Commit != "" {
		fmt.Fprintf(&b, " (%s", short(i.Commit))
		if i.Dirty {
			b.WriteString("-dirty")
		}
		b.WriteString(")")
	} else if i.Dirty {
		b.WriteString(" (dirty)")
	}
	return b.String()
}

// Long is the human-facing report for `outpost version`.
func (i Info) Long() string {
	var b strings.Builder
	fmt.Fprintf(&b, "outpost %s\n", i.Version)
	if i.Commit != "" {
		commit := short(i.Commit)
		if i.Dirty {
			commit += " (working tree had uncommitted changes)"
		}
		fmt.Fprintf(&b, "  commit:   %s\n", commit)
	}
	if i.Date != "" {
		label := "built:   "
		if i.DateIsCommit {
			label = "committed:"
		}
		fmt.Fprintf(&b, "  %s %s\n", label, i.Date)
	}
	fmt.Fprintf(&b, "  go:       %s\n", i.Go)
	fmt.Fprintf(&b, "  platform: %s/%s\n", i.OS, i.Arch)
	return b.String()
}

// Short is the one-line identity used on the wire, where the daemon and the
// CLI compare builds.
func Short() string { return Get().String() }

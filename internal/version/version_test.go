package version

import (
	"strings"
	"testing"
)

// stamp sets the link-time variables for one test and restores them after.
func stamp(t *testing.T, v, c, d string) {
	t.Helper()
	ov, oc, od := Version, Commit, Date
	t.Cleanup(func() { Version, Commit, Date = ov, oc, od })
	Version, Commit, Date = v, c, d
}

// A link-time stamp wins over anything the toolchain recorded: the release
// build knows the tag, the toolchain only knows the commit.
func TestGet_LinkTimeStampWins(t *testing.T) {
	stamp(t, "v1.2.3", "0123456789abcdef0123", "2026-01-02T03:04:05Z")

	got := Get()
	if got.Version != "v1.2.3" {
		t.Errorf("Version = %q; want the linked value", got.Version)
	}
	if got.Commit != "0123456789abcdef0123" {
		t.Errorf("Commit = %q; want the linked value", got.Commit)
	}
	if got.Date != "2026-01-02T03:04:05Z" {
		t.Errorf("Date = %q; want the linked value", got.Date)
	}
	if got.DateIsCommit {
		t.Error("a linked Date was reported as a commit timestamp")
	}
}

// Never claim to be nothing in particular.
func TestGet_AlwaysReportsAVersion(t *testing.T) {
	stamp(t, "", "", "")
	if got := Get().Version; got == "" {
		t.Error("Version is empty; want at least a fallback")
	}
}

func TestGet_ReportsTheBuildPlatform(t *testing.T) {
	got := Get()
	if got.Go == "" || got.OS == "" || got.Arch == "" {
		t.Errorf("incomplete platform: go=%q os=%q arch=%q", got.Go, got.OS, got.Arch)
	}
}

// The one-line form goes in the daemon log and on the wire, so it has to carry
// enough to identify a build without being a paragraph.
func TestInfo_StringCarriesVersionAndCommit(t *testing.T) {
	got := Info{Version: "v1.2.3", Commit: "0123456789abcdef"}.String()
	if want := "v1.2.3 (0123456789ab)"; got != want {
		t.Errorf("String = %q; want %q", got, want)
	}
}

// A dirty build is not the commit it names, and must say so wherever it says
// the commit — otherwise two different binaries report the same identity.
func TestInfo_DirtyIsAlwaysVisible(t *testing.T) {
	withCommit := Info{Version: "v1", Commit: "0123456789abcdef", Dirty: true}
	if got := withCommit.String(); !strings.Contains(got, "dirty") {
		t.Errorf("String = %q; want it marked dirty", got)
	}
	if got := withCommit.Long(); !strings.Contains(got, "uncommitted") {
		t.Errorf("Long = %q; want it marked dirty", got)
	}
	noCommit := Info{Version: "v1", Dirty: true}
	if got := noCommit.String(); !strings.Contains(got, "dirty") {
		t.Errorf("String = %q; want it marked dirty even with no commit", got)
	}
}

// Calling a commit timestamp a build date misleads exactly the person reading
// it during an incident.
func TestInfo_LongDistinguishesCommitTimeFromBuildTime(t *testing.T) {
	built := Info{Version: "v1", Date: "2026-01-02T03:04:05Z"}.Long()
	if !strings.Contains(built, "built:") {
		t.Errorf("Long = %q; want a build date", built)
	}

	committed := Info{Version: "v1", Date: "2026-01-02T03:04:05Z", DateIsCommit: true}.Long()
	if !strings.Contains(committed, "committed:") {
		t.Errorf("Long = %q; want it labelled as a commit time", committed)
	}
	if strings.Contains(committed, "built:") {
		t.Errorf("Long = %q; a commit time must not be called a build date", committed)
	}
}

func TestInfo_LongOmitsWhatItDoesNotKnow(t *testing.T) {
	got := Info{Version: "dev", Go: "go1.26.6", OS: "linux", Arch: "amd64"}.Long()
	for _, absent := range []string{"commit:", "built:", "committed:"} {
		if strings.Contains(got, absent) {
			t.Errorf("Long = %q; want no %q line when it is unknown", got, absent)
		}
	}
	if !strings.Contains(got, "outpost dev") {
		t.Errorf("Long = %q; want the version", got)
	}
}

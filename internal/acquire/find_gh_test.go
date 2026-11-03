package acquire

import (
	"errors"
	"testing"
)

func notOnPath(string) (string, error) { return "", errors.New("not found") }

func TestFindGH_PrefersPATH(t *testing.T) {
	onPath := func(string) (string, error) { return "/custom/bin/gh", nil }
	got := findGH(onPath, func(string) bool { return true }, []string{"/opt/homebrew/bin/gh"})
	if got != "/custom/bin/gh" {
		t.Fatalf("findGH = %q, want the PATH result", got)
	}
}

// Under launchd the PATH is /usr/bin:/bin:/usr/sbin:/sbin, so a Homebrew gh
// is only found by looking where Homebrew puts it.
func TestFindGH_FallsBackToKnownLocations(t *testing.T) {
	present := map[string]bool{"/usr/local/bin/gh": true}
	got := findGH(notOnPath, func(p string) bool { return present[p] },
		[]string{"/opt/homebrew/bin/gh", "/usr/local/bin/gh"})
	if got != "/usr/local/bin/gh" {
		t.Fatalf("findGH = %q, want /usr/local/bin/gh", got)
	}
}

func TestFindGH_NoneFound(t *testing.T) {
	got := findGH(notOnPath, func(string) bool { return false }, ghLocations)
	if got != "" {
		t.Fatalf("findGH = %q, want empty", got)
	}
}

func TestGHLocations_IncludesHomebrew(t *testing.T) {
	for _, p := range ghLocations {
		if p == "/opt/homebrew/bin/gh" {
			return
		}
	}
	t.Fatal("ghLocations is missing /opt/homebrew/bin/gh")
}

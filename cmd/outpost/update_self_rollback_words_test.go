package main

import (
	"strings"
	"testing"
)

// Once a rollback has the old daemon running again, the report mustn't also
// say the services are probably down: that was the state before the
// rollback, not after it, and it is the first thing an admin reads.
func TestUpdateSelf_RolledBackReportDoesntSayDown(t *testing.T) {
	f := newSelfFixture(t)
	f.installJob()
	f.ping.start(oldBuild + " (0123456789ab)")
	newFakeOutpostReleases(t, "v1.1.0", map[string]selfRelease{"v1.1.0": {binary: outpostScript("v1.1.0")}})
	f.onRestart = func(n int) (bool, error) { return n > 1, nil }

	_, _, err := f.update("--wait", "100ms")
	if err == nil {
		t.Fatal("reported success")
	}
	if !strings.Contains(err.Error(), "Rolled back: the daemon is running v1.0.0") {
		t.Fatalf("not rolled back: %v", err)
	}
	if strings.Contains(err.Error(), "probably down") {
		t.Errorf("a rollback that worked still says the services are down: %v", err)
	}
}

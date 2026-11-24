package manifest

import (
	"strings"
	"testing"
)

// A service named outpost would share logs/outpost.log with the daemon.
func TestValidate_DaemonLogNameIsReserved(t *testing.T) {
	errs := ValidateWith([]*Service{portalService(DaemonLogName, "p", 9001)}, Options{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "outpost.log") {
		t.Fatalf("errs = %v; want one saying the name is the daemon log's", errs)
	}
}

package manifest

import (
	"strings"
	"testing"
)

// A service named outpost-access would share logs/outpost-access.log with
// the gateway's access log.
func TestValidate_AccessLogNameIsReserved(t *testing.T) {
	errs := ValidateWith([]*Service{portalService(AccessLogName, "p", 9001)}, Options{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "outpost-access.log") {
		t.Fatalf("errs = %v; want one saying the name is the access log's", errs)
	}
}

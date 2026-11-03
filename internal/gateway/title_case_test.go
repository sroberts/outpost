package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

func TestTitleCase(t *testing.T) {
	for name, want := range map[string]string{
		"tasking":        "Tasking",
		"home-assistant": "Home Assistant",
		"my_app":         "My App",
		"v2.app":         "V2 App",
		"yarr2":          "Yarr2",
		"iOS":            "IOS",
		"ünicode":        "Ünicode",
		"-":              "-",
	} {
		if got := titleCase(name); got != want {
			t.Errorf("titleCase(%q) = %q, want %q", name, got, want)
		}
	}
}

// A manifest's own title is shown as written; only a missing one falls back to
// the title-cased name.
func TestCatalogTitleCasesOnlyAMissingTitle(t *testing.T) {
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "home-assistant", Status: "running", Desired: "running"},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running", Title: "yarr, lowercase on purpose"},
	)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	for _, want := range []string{">Home Assistant<", ">yarr, lowercase on purpose<"} {
		if !strings.Contains(b, want) {
			t.Errorf("catalog lacks %q:\n%s", want, b)
		}
	}
}

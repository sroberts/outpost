package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/version"
)

// Each way an update of Outpost can end gets its own sentence, and only the
// ones that leave something to look at are announced as alerts. "Went back
// to" is said only when something went back.
func TestSelfOutcome(t *testing.T) {
	const old, now = "v1.0.0 (0123456789ab)", "v1.1.0 (fedcba987654)"
	for _, c := range []struct {
		name    string
		st      SelfStatus
		running string
		want    string
		alert   bool
	}{
		{"updated", SelfStatus{To: "v1.1.0", From: old}, now, "Outpost was updated to v1.1.0.", false},
		{"nothing to do", SelfStatus{From: old}, old, "Outpost wasn't changed: it is running v1.0.0.", false},
		{"rolled back", SelfStatus{Failed: true, RolledBack: true, To: "v1.1.0", From: old}, old,
			"v1.1.0 didn't start, so Outpost went back to v1.0.0, which is running.", false},
		{"rolled back somewhere else", SelfStatus{Failed: true, RolledBack: true, To: "v1.1.0", From: old}, now,
			"going back didn't finish cleanly. Outpost is running v1.1.0", true},
		{"refused before changing anything", SelfStatus{Failed: true, To: "v1.1.0", From: old,
			Error: "outpost: release v1.1.0 has no checksum."}, old,
			"Outpost wasn't updated: release v1.1.0 has no checksum. Nothing was changed.", false},
		{"stopped partway", SelfStatus{Failed: true, To: "v1.1.0", From: old}, now,
			"The update stopped partway. Outpost is running v1.1.0", true},
	} {
		got, alert := selfOutcome(c.st, c.running)
		if !strings.Contains(got, c.want) || alert != c.alert {
			t.Errorf("%s: %q alert=%v; want %q alert=%v", c.name, got, alert, c.want, c.alert)
		}
		if c.name == "refused before changing anything" && strings.Contains(got, "went back") {
			t.Errorf("%s says it went back: %q", c.name, got)
		}
	}
}

// The JSON the page polls carries the phase in words, so the page can say
// what's happening as it moves rather than only when it's over.
func TestSelfStatusJSONCarriesThePhaseInWords(t *testing.T) {
	h, f := selfHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	f.set(SelfStatus{Phase: "downloading", To: "v9.0.0"})

	var got struct {
		Phase string `json:"phase"`
		Final bool   `json:"final"`
		Words string `json:"words"`
	}
	if err := json.NewDecoder(h.get(c, portal+"/updates/self").Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Final || got.Words != "Downloading v9.0.0." {
		t.Errorf("got %+v", got)
	}
}

// A finished update shows its sentence, with the whole output behind a
// disclosure rather than as the page's message.
func TestSelfUpdateResultIsASentenceAndALog(t *testing.T) {
	h, f := selfHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()
	f.set(SelfStatus{Phase: "failed", Final: true, Failed: true, To: "v9.0.0", From: version.Get().String(),
		Error: "release v9.0.0 has no build for this platform", Output: "downloading…\nline two"})

	b := body(t, h.get(c, portal+"/updates"))
	for _, want := range []string{"Outpost wasn&#39;t updated: release v9.0.0 has no build for this platform.",
		`<details class="log"><summary>What happened</summary>`, `<pre class="output">downloading…`} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, `role="alert"`) {
		t.Error("an update that changed nothing is announced as an alert")
	}
}

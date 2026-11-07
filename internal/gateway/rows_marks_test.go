package gateway

import (
	"strings"
	"testing"
)

// listRow is the markup of the list row with id, up to its end.
func listRow(t *testing.T, b, id string) string {
	t.Helper()
	i := strings.Index(b, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no row %s", id)
	}
	row := b[i:]
	// The row's own close, past any list nested in it.
	if j := strings.Index(row, "</li>\n"); j > 0 {
		row = row[:j]
	}
	return row
}

// Each app on the Internet access page carries a station saying whether the
// internet reaches it, and the way back to the manifest sits inside Change.
func TestPublicPage_MarksEachAppAndFoldsTheReset(t *testing.T) {
	h, _ := exposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/public"))

	for id, want := range map[string]string{
		"p-tasking": `<span class="smark up"`,
		"p-notes":   `<span class="smark off"`,
	} {
		if row := listRow(t, b, id); !strings.Contains(row, want) {
			t.Errorf("%s lacks %s:\n%s", id, want, row)
		}
	}
	tasking := listRow(t, b, "p-tasking")
	reset := strings.Index(tasking, "Use the manifest&#39;s setting")
	if reset < 0 {
		reset = strings.Index(tasking, "Use the manifest's setting")
	}
	if closes := strings.Index(tasking, "</details>"); reset < 0 || closes < reset {
		t.Errorf("the reset isn't inside Change:\n%s", tasking)
	}
	if !strings.Contains(tasking, `<code class="cmd">https://tasking.tail.ts.net/webhooks/github</code>`) {
		t.Errorf("tasking's public address isn't copyable whole:\n%s", tasking)
	}
}

// A secret no app gets is marked so at a glance, and the add form folds
// away once there are secrets to look at.
func TestSecretsPage_MarksUseAndFoldsTheAddForm(t *testing.T) {
	h, _ := secretsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	if b := body(t, h.get(c, portal+"/secrets")); !strings.Contains(b, `<details class="ref" open>`) {
		t.Error("with no secrets, the add form is folded")
	}
	b := body(t, h.post(c, portal+"/secrets", portal, map[string][]string{
		"action": {"create"}, "name": {"spare"}, "value": {"x"},
	}))
	if row := listRow(t, b, "s-spare"); !strings.Contains(row, `<span class="smark off"`) || !strings.Contains(row, "(not in use)") {
		t.Errorf("a secret no app gets isn't marked so:\n%s", row)
	}
	if strings.Contains(b, `<details class="ref" open>`) {
		t.Error("with a secret to show, the add form is still open")
	}
}

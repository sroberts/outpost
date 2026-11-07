package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
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

// A failed replace opens its form again, with the error tied to the field,
// rather than leaving it above closed pills.
func TestSecretsPage_AFailedReplaceReopensItsForm(t *testing.T) {
	h, _ := secretsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.post(c, portal+"/secrets", portal, map[string][]string{"action": {"create"}, "name": {"k"}, "value": {"x"}})
	b := body(t, h.post(c, portal+"/secrets", portal, map[string][]string{"action": {"rotate"}, "name": {"k"}, "value": {""}}))
	row := listRow(t, b, "s-k")
	if !strings.Contains(row, `name="secret-k" open><summary class="pill"><span>Replace value`) {
		t.Errorf("the failed replace isn't open:\n%s", row)
	}
	if !strings.Contains(row, `aria-describedby="err-s-k"`) || !strings.Contains(row, `id="err-s-k"`) {
		t.Errorf("the error isn't tied to the field:\n%s", row)
	}
}

// What's failing comes first, the page says in a line what the internet
// reaches, and a failing row links to what Funnel needs.
func TestPublicPage_FailingFirstWithATally(t *testing.T) {
	h, _ := exposureHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/webhooks/github"},
			Tailnet:     &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", TailscaleName: "yarr", Funnel: true,
			PublicPaths: []string{"/hooks"},
			Tailnet:     &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelUnavailable, FunnelError: "no funnel attribute"}},
	)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/public"))
	if !strings.Contains(b, "1 on the internet · 1 failing") {
		t.Error("no tally of what the internet reaches")
	}
	if y, k := strings.Index(b, `id="p-yarr"`), strings.Index(b, `id="p-tasking"`); y < 0 || k < 0 || y > k {
		t.Error("the failing app isn't first")
	}
	if row := listRow(t, b, "p-yarr"); !strings.Contains(row, `<a href="#how">What Funnel needs</a>`) {
		t.Errorf("the failing row doesn't link to what Funnel needs:\n%s", row)
	}
	if strings.Contains(b, `class="tag">Public`) {
		t.Error("the PUBLIC tag is back")
	}
}

// With Ollama not answering, a model it serves is marked down, not chosen.
func TestAIPage_OllamaDownMarksItsModelsDown(t *testing.T) {
	h, f := aiHarness(t)
	f.unreachable = true
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/ai"))
	llm := b[strings.Index(b, `id="k-llm"`):strings.Index(b, `id="k-system1"`)]
	if !strings.Contains(llm, `<span class="smark down"`) || !strings.Contains(llm, "(Ollama isn&#39;t answering)") {
		t.Errorf("the LLM looks fine while Ollama is down:\n%s", llm)
	}
}

// An app with Funnel on that isn't running is off, not forever starting.
func TestPublicPage_AStoppedAppIsntStarting(t *testing.T) {
	h, _ := exposureHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "tasking", Status: "stopped", TailscaleName: "tasking", Funnel: true,
		PublicPaths: []string{"/webhooks/github"}})
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/public"))
	if row := listRow(t, b, "p-tasking"); !strings.Contains(row, `<span class="smark off"`) {
		t.Errorf("a stopped app isn't off:\n%s", row)
	}
	if strings.Contains(b, "starting") {
		t.Error("a stopped app is counted as starting")
	}
}

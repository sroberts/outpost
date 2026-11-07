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

// Each app's internet access says how it stands on its line: on the
// internet, failing in red (with what Funnel needs), or not running, rather
// than forever starting.
func TestAppSettings_InternetAccessMarks(t *testing.T) {
	h, _ := exposureHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/webhooks/github"},
			Tailnet:     &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", TailscaleName: "yarr", Funnel: true,
			PublicPaths: []string{"/hooks"},
			Tailnet:     &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelUnavailable, FunnelError: "no funnel attribute"}},
		supervisor.ServiceStatus{Name: "notes", Status: "stopped", TailscaleName: "notes", Funnel: true,
			PublicPaths: []string{"/in"}},
	)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/"))
	panel := func(app string) string {
		p := b[strings.Index(b, `id="set-public-`+app+`"`):]
		return p[:strings.Index(p, "</details>")]
	}
	for app, want := range map[string]string{
		"tasking": `<span class="count">on the internet</span>`,
		"yarr":    `<span class="count why">Funnel isn&#39;t working</span>`,
		"notes":   `<span class="count">app not running</span>`,
	} {
		if p := panel(app); !strings.Contains(p, want) {
			t.Errorf("%s lacks %s:\n%s", app, want, p)
		}
	}
	if p := panel("yarr"); !strings.Contains(p, "Funnel needs the <code>funnel</code> attribute") {
		t.Errorf("the failing app isn't told what Funnel needs:\n%s", p)
	}
}

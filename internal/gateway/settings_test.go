package gateway

import (
	"net/url"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/ai"
)

// The fakes' cheap reads, as an app's Manage menu asks for them.
func (f *fakeAI) Choice(kind string) ai.Choice {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.choices[kind]
}

func (f *fakeAI) Usage() []ai.Usage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.usage
}

func (f *fakeHostedAI) Choice(kind string) ai.Choice {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	return f.choices[kind]
}

// settingsPanel is one panel of an app's settings in the catalog, up to its
// close.
func settingsPanel(t *testing.T, b, id string) string {
	t.Helper()
	i := strings.Index(b, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no panel %s", id)
	}
	p := b[i:]
	// The panel's own close: the last </details> before the next panel or
	// the end of the settings.
	end := len(p)
	for _, stop := range []string{`<details class="setting"`, `</div>` + "\n" + `</details>` + "\n" + `</div>`} {
		if j := strings.Index(p[1:], stop); j >= 0 && j+1 < end {
			end = j + 1
		}
	}
	return p[:end]
}

// What an app has asked of the AI, and its token, are under its menu.
func TestAppSettings_AIUsageAndToken(t *testing.T) {
	h, f := aiHarness(t)
	f.usage = []ai.Usage{{Service: "sous", Kind: ai.LLM, Requests: 3, Errors: 1, InputTokens: 40, OutputTokens: 9,
		LastError: "Ollama can't serve qwen"}}
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/"))
	sous := settingsPanel(t, b, "set-ai-sous")
	for _, want := range []string{"LLM: 3 requests, 1 failed, 40 tokens in, 9 out", "Last error: Ollama can&#39;t serve qwen", "Replace token"} {
		if !strings.Contains(sous, want) {
			t.Errorf("sous's AI panel lacks %q:\n%s", want, sous)
		}
	}
	if tasking := settingsPanel(t, b, "set-ai-tasking"); !strings.Contains(tasking, "No requests yet.") {
		t.Errorf("tasking's AI panel:\n%s", tasking)
	}
	if strings.Contains(sous, `name="kind"`) {
		t.Error("access is offered with no way to set it")
	}

	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-token"}, "app": {"sous"}}))
	if !strings.Contains(b, "New token made.") || !strings.Contains(b, `id="set-ai-sous" open`) {
		t.Errorf("after replacing the token:\n%s", b)
	}
	if strings.Join(f.regenerated, "|") != "sous by root" {
		t.Errorf("regenerated = %v", f.regenerated)
	}
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-token"}, "app": {"nope"}}); resp.StatusCode != 400 {
		t.Errorf("an unknown app: %d", resp.StatusCode)
	}
}

// With hosted providers, an app's menu gives or takes each model from it,
// leaving the other apps as they were.
func TestAppSettings_AIAccess(t *testing.T) {
	h, f := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/"))
	sous := settingsPanel(t, b, "set-ai-sous")
	if !strings.Contains(sous, `name="kind" value="llm" checked`) || !strings.Contains(sous, `name="kind" value="system1" checked`) {
		t.Errorf("sous may use both, but its panel says otherwise:\n%s", sous)
	}
	if tasking := settingsPanel(t, b, "set-ai-tasking"); strings.Contains(tasking, `value="llm" checked`) {
		t.Errorf("tasking isn't given the LLM:\n%s", tasking)
	}

	// Take System 1 (every app) from sous, and give tasking the LLM.
	h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-access"}, "app": {"sous"}, "kind": {"llm"}})
	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-access"}, "app": {"tasking"}, "kind": {"llm", "system1"}}))
	if !strings.Contains(b, "Saved. It takes effect from") {
		t.Errorf("after saving:\n%s", b)
	}
	got := strings.Join(f.apps, "|")
	if !strings.Contains(got, "system1 tasking by root") || !strings.Contains(got, "llm sous,tasking by root") {
		t.Errorf("apps = %s", got)
	}
}

// An app's own secrets are added, replaced and taken away under its menu;
// those shared with every app are shown there, set on the Secrets page.
func TestAppSettings_Secrets(t *testing.T) {
	h, st := secretsHarness(t)
	if _, err := st.Create("shared", "API_KEY", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Bind("shared", ""); err != nil {
		t.Fatal(err)
	}
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.post(c, portal+"/settings", portal, url.Values{
		"action": {"secret-add"}, "app": {"sous"}, "name": {"sous-key"}, "env": {"API_KEY"}, "value": {distinctive},
	}))
	sous := settingsPanel(t, b, "set-secrets-sous")
	for _, want := range []string{"Added sous-key. Sous restarts with it", "<code>API_KEY</code> <span class=\"hint\">sous-key</span>", "Sous's own wins"} {
		if !strings.Contains(sous, want) {
			t.Errorf("sous's secrets lack %q:\n%s", want, sous)
		}
	}
	if strings.Contains(b, distinctive) {
		t.Fatal("the page shows a secret's value")
	}
	if vars, _ := st.Resolve("sous"); vars["API_KEY"] != distinctive {
		t.Errorf("sous gets %v", vars)
	}
	if yarr := settingsPanel(t, b, "set-secrets-yarr"); strings.Contains(yarr, "sous-key") {
		t.Error("yarr is shown sous's own secret")
	}
	if b := body(t, h.get(c, portal+"/secrets")); strings.Contains(b, `id="s-sous-key"`) {
		t.Error("the Secrets page lists one app's own secret")
	}

	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"secret-replace"}, "app": {"sous"}, "name": {"sous-key"}, "value": {"2"}}))
	if !strings.Contains(settingsPanel(t, b, "set-secrets-sous"), "Replaced sous-key.") {
		t.Error("no confirmation of the replacement on sous's panel")
	}
	if vars, _ := st.Resolve("sous"); vars["API_KEY"] != "2" {
		t.Errorf("replacement didn't take: %v", vars)
	}

	// A refusal comes back open on the panel, keeping the name.
	resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"secret-add"}, "app": {"yarr"}, "name": {"x"}, "env": {"OUTPOST_PORT"}, "value": {distinctive}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "set by Outpost itself") || !strings.Contains(b, `id="set-secrets-yarr" open`) || strings.Contains(b, distinctive) {
		t.Errorf("a refused add: %d\n%s", resp.StatusCode, b)
	}

	b = body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"secret-remove"}, "app": {"sous"}, "name": {"sous-key"}}))
	if !strings.Contains(b, "Took sous-key away from Sous.") {
		t.Errorf("after removing:\n%s", b)
	}
	if _, ok := st.Get("sous-key"); ok {
		t.Error("a secret no app has any more was kept")
	}
}

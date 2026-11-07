package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/ai"
)

// The Providers and Apps sections fold away unless something in them needs
// the admin: a chosen model with no key, or what a form there just did.
func TestAIPage_Hosted_FoldsWhatNeedsNothing(t *testing.T) {
	h, f := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	// The fake's LLM is OpenAI's, with no key: Providers opens to add one.
	b := body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, `<details class="ref" open><summary><h2 id="prov-title">`) {
		t.Error("Providers is folded while the LLM has no key")
	}
	if !strings.Contains(b, "2 of 4 keys set") {
		t.Error("Providers' summary doesn't count the keys")
	}
	if !strings.Contains(b, `<details class="ref"><summary><h2 id="apps-title">`) {
		t.Error("Apps is open with nothing to show")
	}

	f.hmu.Lock()
	llm := f.choices[ai.LLM]
	llm.NoKey = false
	f.choices[ai.LLM] = llm
	f.hmu.Unlock()
	b = body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, `<details class="ref"><summary><h2 id="prov-title">`) {
		t.Error("Providers is open with nothing wanting a key")
	}

	// Coming back from a form there opens its section.
	b = body(t, h.get(c, portal+"/ai?done=key&provider=openrouter"))
	if !strings.Contains(b, `<details class="ref" open><summary><h2 id="prov-title">`) {
		t.Error("Providers is folded over the key just saved")
	}
	b = body(t, h.get(c, portal+"/ai?done=regenerated&app=sous"))
	if !strings.Contains(b, `<details class="ref" open><summary><h2 id="apps-title">`) {
		t.Error("Apps is folded over the token just replaced")
	}
}

package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/ai"
)

// The Providers section folds away unless something in it needs
// the admin: a chosen model with no key, or what a form there just did.
func TestAIPage_Hosted_FoldsWhatNeedsNothing(t *testing.T) {
	h, f := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	// The fake's LLM is OpenAI's, with no key: Providers opens to add one.
	b := body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, `<h2 id="prov-title">Providers</h2>`+"\n"+`<details class="ref" open>`) {
		t.Error("Providers is folded while the LLM has no key")
	}
	if !strings.Contains(b, "2 of 4 keys set") {
		t.Error("Providers' summary doesn't count the keys")
	}

	f.hmu.Lock()
	llm := f.choices[ai.LLM]
	llm.NoKey = false
	f.choices[ai.LLM] = llm
	f.hmu.Unlock()
	b = body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, `<h2 id="prov-title">Providers</h2>`+"\n"+`<details class="ref">`) {
		t.Error("Providers is open with nothing wanting a key")
	}

	// Coming back from a form there opens it.
	b = body(t, h.get(c, portal+"/ai?done=key&provider=openrouter"))
	if !strings.Contains(b, `<h2 id="prov-title">Providers</h2>`+"\n"+`<details class="ref" open>`) {
		t.Error("Providers is folded over the key just saved")
	}
}

// A model that can only fail says so at a glance, offers no Test, and its
// link to the missing key lands on that provider's key form, which says
// what it's holding up.
func TestAIPage_Hosted_ABrokenModelPointsAtItsFix(t *testing.T) {
	h, _ := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/ai"))
	llm := b[strings.Index(b, `id="k-llm"`):strings.Index(b, `id="k-system1"`)]
	sys := b[strings.Index(b, `id="k-system1"`):strings.Index(b, `id="providers"`)]
	for _, want := range []string{`<span class="smark down"`, "(not working)", `href="#key-openai"`} {
		if !strings.Contains(llm, want) {
			t.Errorf("the keyless LLM lacks %q", want)
		}
	}
	if strings.Contains(llm, `value="test"`) {
		t.Error("the keyless LLM offers a Test that can only fail")
	}
	if !strings.Contains(sys, `<span class="smark up"`) || !strings.Contains(sys, `value="test"`) {
		t.Error("the working System 1 isn't marked up, or has no Test")
	}
	if !strings.Contains(b, `id="key-openai"`) || !strings.Contains(b, "The LLM is waiting for this key.") {
		t.Error("OpenAI's row doesn't say the LLM is waiting on it")
	}
}

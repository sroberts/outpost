package gateway

import (
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/secrets"
	"github.com/sroberts/outpost/internal/supervisor"
)

func secretsHarness(t *testing.T) (*harness, *secrets.Store) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	st, err := secrets.Open(filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.g.secrets = st
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Title: "Sous", Status: "running", Desired: "running"},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", Desired: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, st
}

const distinctive = "sk-live-0123456789-distinctive"

// An admin adds a secret for one app, rotates it, gives it to every app and
// removes it, and the page never once shows a value.
func TestSecretsPage_AddRotateBindRemove(t *testing.T) {
	h, st := secretsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/secrets"`) {
		t.Errorf("the catalog doesn't link an admin to secrets:\n%s", b)
	}

	b := body(t, h.post(c, portal+"/secrets", portal, url.Values{
		"action": {"create"}, "name": {"openai-key"}, "value": {distinctive}, "app": {"yarr"},
	}))
	for _, want := range []string{"Added openai-key.", "Yarr restarts with it", "OPENAI_KEY", "Given to Yarr"} {
		if !strings.Contains(b, want) {
			t.Errorf("after adding, page lacks %q:\n%s", want, b)
		}
	}
	if vars, _ := st.Resolve("yarr"); vars["OPENAI_KEY"] != distinctive {
		t.Errorf("yarr gets %v", vars)
	}
	if vars, _ := st.Resolve("sous"); len(vars) != 0 {
		t.Errorf("sous got a secret given only to yarr: %v", vars)
	}

	b = body(t, h.post(c, portal+"/secrets", portal, url.Values{
		"action": {"rotate"}, "name": {"openai-key"}, "value": {distinctive + "-2"},
	}))
	if !strings.Contains(b, "Rotated openai-key.") {
		t.Errorf("after rotating:\n%s", b)
	}
	if vars, _ := st.Resolve("yarr"); vars["OPENAI_KEY"] != distinctive+"-2" {
		t.Errorf("rotation didn't take: %v", vars)
	}

	b = body(t, h.post(c, portal+"/secrets", portal, url.Values{
		"action": {"apps"}, "name": {"openai-key"}, "global": {"1"},
	}))
	if !strings.Contains(b, "Given to every app") || !strings.Contains(b, "Sous and Yarr") {
		t.Errorf("after giving it to every app:\n%s", b)
	}
	if sec, _ := st.Get("openai-key"); !sec.Global || len(sec.Services) != 0 {
		t.Errorf("bindings = %+v; want every app and no longer yarr by name", sec)
	}
	if strings.Contains(b, distinctive) {
		t.Fatal("the page shows a secret's value")
	}

	b = body(t, h.post(c, portal+"/secrets", portal, url.Values{"action": {"remove"}, "name": {"openai-key"}}))
	if !strings.Contains(b, "Removed openai-key.") || !strings.Contains(b, "No secrets yet.") {
		t.Errorf("after removing:\n%s", b)
	}
	if len(st.List()) != 0 {
		t.Error("still stored after removal")
	}
}

// A refusal re-shows the form with what was typed, less the value, and says
// why, in the store's words.
func TestSecretsPage_RefusalsKeepTheFormButNotTheValue(t *testing.T) {
	h, st := secretsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	resp := h.post(c, portal+"/secrets", portal, url.Values{
		"action": {"create"}, "name": {"new-key"}, "env": {"OUTPOST_PORT"}, "value": {distinctive},
	})
	b := body(t, resp)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "set by Outpost itself") {
		t.Errorf("an OUTPOST_ variable: %d\n%s", resp.StatusCode, b)
	}
	if !strings.Contains(b, `value="new-key"`) || strings.Contains(b, distinctive) {
		t.Errorf("the form should keep the name and drop the value:\n%s", b)
	}

	if _, err := st.Create("a", "SAME", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Bind("a", "sous"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create("b", "SAME", "2"); err != nil {
		t.Fatal(err)
	}
	resp = h.post(c, portal+"/secrets", portal, url.Values{"action": {"apps"}, "name": {"b"}, "app": {"sous"}})
	if b := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "already gets SAME from secret a") {
		t.Errorf("a clashing binding: %d\n%s", resp.StatusCode, b)
	}
	if sec, _ := st.Get("b"); len(sec.Services) != 0 {
		t.Errorf("the refused binding was kept: %+v", sec)
	}
}

func TestSecretsPage_OnlyAnAdmin(t *testing.T) {
	h, st := secretsHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, `href="/secrets"`) {
		t.Error("the catalog links someone who isn't an admin to secrets")
	}
	if resp := h.get(plain, portal+"/secrets"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET as a plain user = %d", resp.StatusCode)
	}
	create := url.Values{"action": {"create"}, "name": {"k"}, "value": {"v"}}
	if resp := h.post(plain, portal+"/secrets", portal, create); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}

	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/secrets", "http://evil.test", create); resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST from another site = %d", resp.StatusCode)
	}

	anon := h.client(false)
	if resp := h.get(anon, portal+"/secrets"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Errorf("signed out: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if len(st.List()) != 0 {
		t.Error("a refused request created a secret")
	}
}

func TestNoSecretsPageWithoutAStore(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/")); strings.Contains(b, `href="/secrets"`) {
		t.Error("the catalog links to a secrets page that isn't there")
	}
	if resp := h.get(c, portal+"/secrets"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /secrets with no store = %d", resp.StatusCode)
	}
}

func TestListWords(t *testing.T) {
	for want, in := range map[string][]string{
		"":           nil,
		"a":          {"a"},
		"a and b":    {"a", "b"},
		"a, b and c": {"a", "b", "c"},
	} {
		if got := listWords(in); got != want {
			t.Errorf("listWords(%v) = %q, want %q", in, got, want)
		}
	}
}

// A secret given to every app says which apps have their own instead, and a
// change's confirmation is on the row the redirect lands on.
func TestSecretsPage_ShadowedAppsAndRowNotice(t *testing.T) {
	h, st := secretsHarness(t)
	for _, step := range []func() (secrets.Secret, error){
		func() (secrets.Secret, error) { return st.Create("shared", "API_KEY", "1") },
		func() (secrets.Secret, error) { return st.Bind("shared", "") },
		func() (secrets.Secret, error) { return st.Create("sous-own", "API_KEY", "2") },
		func() (secrets.Secret, error) { return st.Bind("sous-own", "sous") },
	} {
		if _, err := step(); err != nil {
			t.Fatal(err)
		}
	}
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.get(c, portal+"/secrets"))
	if !strings.Contains(b, "In use by Yarr; Sous has its own API_KEY") {
		t.Errorf("the shared secret doesn't say Sous has its own:\n%s", b)
	}

	b = body(t, h.post(c, portal+"/secrets", portal, url.Values{"action": {"rotate"}, "name": {"sous-own"}, "value": {"3"}}))
	row := b[strings.Index(b, `id="s-sous-own"`):]
	if i := strings.Index(row, "</li>"); i > 0 {
		row = row[:i]
	}
	if !strings.Contains(row, "Rotated sous-own. Sous restarts with it") {
		t.Errorf("the rotation's confirmation isn't on its row:\n%s", row)
	}
}

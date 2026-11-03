package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeAdds stands in for the daemon's `outpost add`.
type fakeAdds struct {
	mu         sync.Mutex
	previews   []string
	adds       []string
	previewOut string
	previewErr error
	gate       chan struct{} // when set, Add waits on it
	output     string
	err        error
}

func (f *fakeAdds) Preview(_ context.Context, repo string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previews = append(f.previews, repo)
	return f.previewOut, f.previewErr
}

func (f *fakeAdds) Add(repo string) (string, error) {
	f.mu.Lock()
	f.adds = append(f.adds, repo)
	gate := f.gate
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.output, f.err
}

func (f *fakeAdds) calls() (previews, adds []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.previews...), append([]string(nil), f.adds...)
}

// addSettled waits for a background add to finish; the test log is off limits
// once a test returns, and it logs.
func (h *harness) addSettled() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.g.addState.mu.Lock()
		busy := h.g.addState.running != ""
		h.g.addState.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatal("the add never settled")
}

func addHarness(t *testing.T) (*harness, *fakeAdds) {
	t.Helper()
	h := newHarness(t)
	f := &fakeAdds{previewOut: "would install tasking-darwin-arm64 from release v0.1.0\nname = \"tasking\"\n"}
	h.g.adder = f
	t.Cleanup(h.addSettled)
	h.withServices(supervisor.ServiceStatus{Name: "wiki", Status: "running", Desired: "running"})
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

func addForm(action, repo string) url.Values {
	return url.Values{"action": {action}, "repo": {repo}}
}

// Pasting a link looks the repository up and shows what adding it would do;
// only the button on that preview installs it, and it's given owner/name.
func TestAddLooksUpThenAdds(t *testing.T) {
	h, f := addHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/add"`) {
		t.Errorf("the catalog doesn't link an admin to adding an app:\n%s", b)
	}
	if b := body(t, h.get(c, portal+"/add")); !strings.Contains(b, `name="repo"`) || !strings.Contains(b, `value="preview"`) {
		t.Errorf("the add page has no repository box:\n%s", b)
	}

	b := body(t, h.post(c, portal+"/add", portal, addForm("preview", "https://github.com/sroberts/tasking/releases/tag/v0.1.0")))
	for _, want := range []string{"would install tasking-darwin-arm64", `name="repo" value="sroberts/tasking"`, `value="add"`, "Nothing has been downloaded yet"} {
		if !strings.Contains(b, want) {
			t.Errorf("preview lacks %q:\n%s", want, b)
		}
	}
	if previews, adds := f.calls(); len(previews) != 1 || previews[0] != "sroberts/tasking" || len(adds) != 0 {
		t.Fatalf("after the preview: previews %v, adds %v; want one preview of sroberts/tasking and no add", previews, adds)
	}

	f.gate = make(chan struct{})
	f.output = "installed /home/bin/tasking (checksum verified)\nwrote /home/services/tasking.toml\n"
	resp := h.post(c, portal+"/add", portal, addForm("add", "sroberts/tasking"))
	b = body(t, resp)
	if resp.Request.URL.Path != "/add" || !strings.Contains(b, "Adding sroberts/tasking.") {
		t.Errorf("while adding: %s\n%s", resp.Request.URL, b)
	}
	if !strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the page doesn't refresh while an add runs")
	}
	// A second tap while it runs starts nothing.
	h.post(c, portal+"/add", portal, addForm("add", "sroberts/tasking"))
	close(f.gate)
	h.addSettled()
	if _, adds := f.calls(); len(adds) != 1 || adds[0] != "sroberts/tasking" {
		t.Errorf("adds run = %v; want sroberts/tasking, once", adds)
	}

	b = body(t, h.get(c, portal+"/add"))
	for _, want := range []string{"Added sroberts/tasking", "checksum verified"} {
		if !strings.Contains(b, want) {
			t.Errorf("after the add, page lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(b, `http-equiv="refresh"`) {
		t.Error("the page keeps refreshing with nothing running")
	}
}

func TestAddShowsWhyItFailed(t *testing.T) {
	h, f := addHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	f.previewErr = errors.New("sroberts/nothing has no published release")
	b := body(t, h.post(c, portal+"/add", portal, addForm("preview", "sroberts/nothing")))
	if !strings.Contains(b, "has no published release") || strings.Contains(b, `value="add"`) {
		t.Errorf("a failed lookup should explain itself and offer no add:\n%s", b)
	}

	f.err = errors.New("/home/services/tasking.toml already exists (use --force to overwrite)")
	h.post(c, portal+"/add", portal, addForm("add", "sroberts/tasking"))
	h.addSettled()
	b = body(t, h.get(c, portal+"/add"))
	if !strings.Contains(b, "Couldn't add sroberts/tasking") || !strings.Contains(b, "already exists") {
		t.Errorf("a failed add should say so:\n%s", b)
	}
}

// Only a GitHub repository reaches the add command, and only as owner/name:
// nothing that could be read as a flag, a path or another host.
func TestAddRefusesWhatIsntARepository(t *testing.T) {
	h, f := addHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	for _, repo := range []string{"", "tasking", "https://gitlab.com/o/r", "-x/r", "o/--force", "../../etc", "file:///etc/passwd"} {
		for _, action := range []string{"preview", "add"} {
			resp := h.post(c, portal+"/add", portal, addForm(action, repo))
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("%s %q = %d, want 400", action, repo, resp.StatusCode)
			}
		}
	}
	if previews, adds := f.calls(); len(previews)+len(adds) != 0 {
		t.Errorf("refused repositories reached the add command: previews %v, adds %v", previews, adds)
	}
}

func TestOnlyAnAdminCanAdd(t *testing.T) {
	h, f := addHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")

	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, `href="/add"`) {
		t.Errorf("the catalog links someone who isn't an admin to adding apps:\n%s", b)
	}
	if resp := h.get(plain, portal+"/add"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET as a plain user = %d", resp.StatusCode)
	}
	for _, action := range []string{"preview", "add"} {
		if resp := h.post(plain, portal+"/add", portal, addForm(action, "o/r")); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s as a plain user = %d", action, resp.StatusCode)
		}
	}

	admin := h.client(true)
	h.login(admin, "root", "", "/")
	for _, action := range []string{"preview", "add"} {
		if resp := h.post(admin, portal+"/add", "http://evil.test", addForm(action, "o/r")); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s from another site = %d", action, resp.StatusCode)
		}
	}

	anon := h.client(false)
	if resp := h.get(anon, portal+"/add"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "/login") {
		t.Errorf("signed out: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if previews, adds := f.calls(); len(previews)+len(adds) != 0 {
		t.Errorf("refused requests reached the add command: previews %v, adds %v", previews, adds)
	}
}

func TestNoAddPageWithoutAnAdder(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/")); strings.Contains(b, `href="/add"`) {
		t.Errorf("the catalog links to an add page that isn't there:\n%s", b)
	}
	if resp := h.get(c, portal+"/add"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /add with no adder = %d", resp.StatusCode)
	}
}

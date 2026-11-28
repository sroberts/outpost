package tui

import (
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/ipc"
)

// Every call the socket offers is reachable from the dashboard, so the TUI
// can't fall behind the CLI and the portal again without this failing.
func TestParity_EveryClientMethodIsReachable(t *testing.T) {
	covers := Covers()
	typ := reflect.TypeOf(&ipc.Client{})
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		if !covers["ipc:"+name] {
			t.Errorf("(*ipc.Client).%s is not reachable from the dashboard: add an action covering ipc:%s", name, name)
		}
	}
}

// Everything an action covers exists, so a renamed method can't leave a
// stale claim behind.
func TestParity_CoversNameRealMethods(t *testing.T) {
	typ := reflect.TypeOf(&ipc.Client{})
	for c := range Covers() {
		name, ok := strings.CutPrefix(c, "ipc:")
		if !ok {
			continue
		}
		if _, ok := typ.MethodByName(name); !ok {
			t.Errorf("%s names no (*ipc.Client) method", c)
		}
	}
}

// Every action can be opened from the palette, filled in and confirmed
// without panicking, on a client with nothing behind it.
func TestParity_ActionsHaveUniqueIDsAndOpen(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range actions() {
		if seen[a.id] {
			t.Errorf("action id %q is used twice", a.id)
		}
		seen[a.id] = true
		if a.run == nil || a.title == "" || a.group == "" {
			t.Errorf("action %q is missing its run, title or group", a.id)
		}
		if len(a.covers) == 0 {
			t.Errorf("action %q covers nothing", a.id)
		}
	}
}

func TestPalette_FindsFillsAndConfirms(t *testing.T) {
	c := newSecretsClient()
	m := loadedFull(t, c)
	m = press(t, m, ":")
	if !m.pal.open {
		t.Fatal(": did not open the palette")
	}
	for _, r := range "delete secret" {
		m = press(t, m, string(r))
	}
	if v := m.View(); !strings.Contains(v, "delete secret") {
		t.Fatalf("palette doesn't offer it:\n%s", v)
	}
	m = press(t, m, "enter")
	if m.pal.form == nil {
		t.Fatal("enter didn't open the form")
	}
	for _, r := range "db-pass" {
		m = press(t, m, string(r))
	}
	m = press(t, m, "enter")
	if !m.pal.form.confirming || !strings.Contains(m.View(), "Delete secret db-pass?") {
		t.Fatalf("no confirmation asked:\n%s", m.View())
	}
	var cmd tea.Cmd
	m, cmd = run(t, m, keyMsg("y"))
	m = drain(t, m, cmd)
	if len(c.removed) != 1 || c.removed[0] != "db-pass" {
		t.Fatalf("SecretRemove calls = %v", c.removed)
	}
}

func TestPalette_AnyKeyButYCancels(t *testing.T) {
	c := newSecretsClient()
	m := loadedFull(t, c)
	m, _ = m.openAction("secret-rm")
	m = press(t, m, "x", "enter", "n")
	if m.pal.form != nil || len(c.removed) != 0 {
		t.Fatalf("a no went ahead: form=%v removed=%v", m.pal.form, c.removed)
	}
}

func TestPalette_SecretsAreNeverShown(t *testing.T) {
	c := newSecretsClient()
	m := loadedFull(t, c)
	m, _ = m.openAction("secret-set")
	m = press(t, m, "n", "enter", "enter")
	for _, r := range "hunter2" {
		m = press(t, m, string(r))
	}
	if v := m.View(); strings.Contains(v, "hunter2") {
		t.Fatalf("a secret value is on screen:\n%s", v)
	}
	m = press(t, m, "enter")
	if v := m.View(); strings.Contains(v, "hunter2") {
		t.Fatalf("a secret value is on screen after moving on:\n%s", v)
	}
}

// The palette takes q and esc while it's up, so typing a word with a q in
// it doesn't quit.
func TestPalette_TypingQDoesNotQuit(t *testing.T) {
	m := loadedFull(t, newSecretsClient())
	m = press(t, m, ":", "q")
	if m.quitting || !m.pal.open {
		t.Fatal("q in the palette quit the dashboard")
	}
	m = press(t, m, "esc")
	if m.pal.open || m.quitting {
		t.Fatal("esc should close the palette and only that")
	}
}

func TestPalette_RunnerOutputIsShownAndOffersTheAdd(t *testing.T) {
	c := newSecretsClient()
	var ran [][]string
	m := New(c, nil).WithRunner(func(args ...string) (string, error) {
		ran = append(ran, args)
		return "would install owner/app v1.0\n", nil
	})
	m.width, m.height = 100, 40
	m = drain(t, m, m.fetchServices())
	m, _ = m.openAction("add")
	for _, r := range "owner/app" {
		m = press(t, m, string(r))
	}
	var cmd tea.Cmd
	m, cmd = run(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	m = drain(t, m, cmd)
	if m.out == nil || !strings.Contains(m.View(), "would install owner/app") {
		t.Fatalf("preview not shown:\n%s", m.View())
	}
	m, cmd = run(t, m, keyMsg("a"))
	m = drain(t, m, cmd)
	if len(ran) != 2 || strings.Join(ran[0], " ") != "add --print owner/app" || strings.Join(ran[1], " ") != "add owner/app" {
		t.Fatalf("ran %v", ran)
	}
}

// A purge is asked about here, so the command's own prompt, which would read
// the dashboard's keyboard, is never reached.
func TestPalette_PurgePassesYes(t *testing.T) {
	var ran []string
	m := New(newSecretsClient(), nil).WithRunner(func(args ...string) (string, error) {
		ran = args
		return "", nil
	})
	m.width, m.height = 100, 40
	m = drain(t, m, m.fetchServices())
	m, _ = m.openAction("remove")
	m = press(t, m, "enter", " ", "enter")
	if !strings.Contains(m.View(), "permanently delete") {
		t.Fatalf("purge not called out:\n%s", m.View())
	}
	var cmd tea.Cmd
	m, cmd = run(t, m, keyMsg("y"))
	_ = drain(t, m, cmd)
	if strings.Join(ran, " ") != "remove notes --purge --yes" {
		t.Fatalf("ran %v", ran)
	}
}

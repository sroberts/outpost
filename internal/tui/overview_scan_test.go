package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/stats"
)

// backupsClient lists backups for every service, or fails to when told to.
type backupsClient struct {
	*fullClient
	fail  bool
	reads int
}

func (c *backupsClient) Backups(n string) ([]backup.Archive, error) {
	c.reads++
	if c.fail {
		return nil, errors.New("daemon unreachable")
	}
	return c.fullClient.Backups(n)
}

func newBackupsClient() *backupsClient {
	c := &backupsClient{fullClient: newSecretsClient()}
	for i := range c.services {
		c.services[i].Storage = "/srv/" + c.services[i].Name
	}
	return c
}

// The overview opens before the service list arrives; that first read must
// not count as having found no backups for the next five minutes.
func TestOverview_BackupsReadOnceTheListHasLoaded(t *testing.T) {
	c := newBackupsClient()
	m := New(c, nil).WithOverview()
	m.width, m.height = 120, 40
	m = drain(t, m, m.fetchOverview())
	if c.reads != 0 {
		t.Fatalf("read backups for %d services before the list loaded", c.reads)
	}
	m = drain(t, m, m.fetchServices())
	m = drain(t, m, m.fetchOverview())
	if c.reads == 0 {
		t.Fatal("backups never read once the services had loaded")
	}
	if _, ok := m.ov.backups[c.services[0].Name]; !ok {
		t.Fatalf("no backup recorded for %s: %v", c.services[0].Name, m.ov.backups)
	}
	if v := m.View(); strings.Contains(v, "never") {
		t.Errorf("overview shows a service with backups as never:\n%s", v)
	}

	// Fresh now: the next refresh doesn't read them again.
	before := c.reads
	m = drain(t, m, m.fetchOverview())
	if c.reads != before {
		t.Errorf("re-read backups %d times within backupsEvery", c.reads-before)
	}
}

// A read that fails keeps what the last one found, and isn't taken as fresh.
func TestOverview_FailedBackupReadKeepsTheLast(t *testing.T) {
	c := newBackupsClient()
	m := New(c, nil).WithOverview()
	m.width, m.height = 120, 40
	m = drain(t, m, m.fetchServices())
	m = drain(t, m, m.fetchOverview())
	name := c.services[0].Name
	had := m.ov.backups[name]

	m.ov.backupAt = time.Now().Add(-2 * backupsEvery)
	c.fail = true
	m = drain(t, m, m.fetchOverview())
	if got, ok := m.ov.backups[name]; !ok || !got.Equal(had) {
		t.Errorf("after a failed read %s's backup = %v (present %v), want %v", name, got, ok, had)
	}
	before := c.reads
	m = drain(t, m, m.fetchOverview())
	if c.reads == before {
		t.Error("a failed read counted as fresh; the next refresh did not retry")
	}
}

// A running service that hasn't been measured yet isn't "down".
func TestGraphs_UnmeasuredRunningServiceIsNotDown(t *testing.T) {
	c := newSecretsClient()
	c.usage = map[string]stats.Usage{"notes": {Up: true, Measured: false}}
	m := loadedFull(t, c)
	for _, l := range m.graphLines("notes", 80, 1) {
		if strings.Contains(plainText(l), "down") {
			t.Errorf("unmeasured running service drawn as down: %q", plainText(l))
		}
	}
	c.usage = map[string]stats.Usage{"notes": {Up: false}}
	m = loadedFull(t, c)
	found := false
	for _, l := range m.graphLines("notes", 80, 1) {
		found = found || strings.Contains(plainText(l), "down")
	}
	if !found {
		t.Error("a stopped service isn't drawn as down")
	}
}

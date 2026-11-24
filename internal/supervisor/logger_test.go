package supervisor

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/logging"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Handed a logger, the supervisor logs each decision at a level with the
// service as a field, in the words it always used.
func TestLogger_DecisionsAreLeveledWithFields(t *testing.T) {
	h := newHarness(t)
	var out syncBuffer
	sup, err := New(Options{
		ServicesDir:    filepath.Join(h.dir, "services"),
		Store:          h.store,
		ProcessRuntime: h.rt,
		Now:            h.clock.Now,
		Logger:         logging.Component(logging.New(&out, "debug", "logfmt"), "supervisor", "debug"),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.sup = sup
	h.writeManifest("echo", "")
	h.apply()
	h.tick()

	got := out.String()
	for _, want := range []string{"level=info", "prefix=supervisor", `msg="started echo`, "service=echo", "action=start"} {
		if !strings.Contains(got, want) {
			t.Errorf("log lacks %q:\n%s", want, got)
		}
	}
}

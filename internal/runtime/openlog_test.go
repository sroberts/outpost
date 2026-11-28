package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// The daemon's log is owner-only, appended to across restarts, and rolls
// over like a service's.
func TestOpenLog_AppendsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "outpost.log")
	for _, line := range []string{"one\n", "two\n"} {
		w, err := OpenLog(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "one\ntwo\n" {
		t.Errorf("log = %q", body)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v; want 0600", fi.Mode().Perm())
	}
	if w, ok := mustOpen(t, path).(*rotatingWriter); !ok || w.maxBytes != defaultMaxLogBytes || w.backups != defaultLogBackups {
		t.Errorf("not rotated as a service's log is: %+v", w)
	}
}

func mustOpen(t *testing.T, path string) any {
	t.Helper()
	w, err := OpenLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() }) // a test file; nothing to lose
	return w
}

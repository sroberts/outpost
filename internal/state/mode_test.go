package state

import (
	"os"
	"path/filepath"
	"testing"
)

// state.db names every service and its activity, so it is owner-only like
// auth.db, whatever the umask, and so is the WAL SQLite gives its mode.
func TestOpen_CreatesOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Force a write, so the WAL file exists too.
	if err := s.AppendEvent("svc", EventRestarted, "test"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if mode := fi.Mode().Perm(); mode != 0o600 {
			t.Errorf("%s has mode %o; want 600", filepath.Base(p), mode)
		}
	}
}

func TestOpen_TightensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("state.db has mode %o after Open; want 600", mode)
	}
}

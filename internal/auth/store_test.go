package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clock is a settable time source for expiry tests.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

// newTestStore opens a store in a temp dir with fast hashing and a fake clock.
func newTestStore(t *testing.T) (*Store, *clock) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c := &clock{t: time.Unix(1_800_000_000, 0)}
	s.Params = testParams
	s.now = c.now
	return s, c
}

func TestOpen_CreatesOwnerOnlyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Force a write, so the WAL file exists too.
	if err := s.AddUser(User{Username: "alice"}, ""); err != nil {
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
	path := filepath.Join(t.TempDir(), "auth.db")
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
		t.Errorf("auth.db has mode %o after Open; want 600", mode)
	}
}

func TestOpen_ReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Params = testParams
	if err := s.AddUser(User{Username: "alice"}, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s.Close()
	if _, err := s.Authenticate("alice", "correct horse battery"); err != nil {
		t.Errorf("alice can't log in after a reopen: %v", err)
	}
}

func TestOpen_RecordsSchemaVersion(t *testing.T) {
	s, _ := newTestStore(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != "1" {
		t.Errorf("schema version = %q; want 1", v)
	}
}

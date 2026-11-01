package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// The snapshot is what an operator restores from after an update goes wrong,
// so it has to hold what the live database held, including writes still in
// the WAL of a store that is open, and be no more readable than auth.db.
func TestSnapshot_CopiesAnOpenStore(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "auth.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Params = testParams
	if err := s.AddUser(User{Username: "alice", Admin: true}, "correct horse battery"); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "snap.db")
	if err := Snapshot(src, dst); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("snapshot mode %o; want 0600", mode)
	}

	copied, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	copied.Params = testParams
	u, err := copied.Authenticate("alice", "correct horse battery")
	if err != nil {
		t.Fatalf("the snapshot doesn't hold alice: %v", err)
	}
	if !u.Admin {
		t.Error("the snapshot lost alice's admin flag")
	}
}

// A snapshot never replaces anything, least of all an earlier snapshot.
func TestSnapshot_RefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "auth.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	dst := filepath.Join(dir, "snap.db")
	if err := os.WriteFile(dst, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(src, dst); err == nil {
		t.Fatal("Snapshot overwrote an existing file")
	}
	if got, _ := os.ReadFile(dst); string(got) != "keep me" {
		t.Errorf("existing file now holds %q", got)
	}
}

func TestSnapshot_MissingSourceLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "snap.db")
	if err := Snapshot(filepath.Join(dir, "absent.db"), dst); err == nil {
		t.Fatal("Snapshot of a missing database succeeded")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("a failed snapshot left %s behind: %v", dst, err)
	}
	// Opening read-only must not have created the source either.
	if _, err := os.Stat(filepath.Join(dir, "absent.db")); !os.IsNotExist(err) {
		t.Errorf("the source was created: %v", err)
	}
}

// With the daemon stopped there is no -shm file to share, and a read-only open
// can't make one; the snapshot has to work then too.
func TestSnapshot_CopiesAClosedStore(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "auth.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{Username: "bob"}, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	dst := filepath.Join(dir, "snap.db")
	if err := Snapshot(src, dst); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	copied, err := Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	if _, err := copied.GetUser("bob"); err != nil {
		t.Errorf("the snapshot doesn't hold bob: %v", err)
	}
}

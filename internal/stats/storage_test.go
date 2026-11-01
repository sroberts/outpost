package stats

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func measureAll(t *testing.T, root string, maxEntries int) usage {
	t.Helper()
	u, err := measure(context.Background(), root, time.Minute, maxEntries, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMeasure_DoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "big"), make([]byte, 4<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "small"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"file": filepath.Join(outside, "big"),
		"dir":  outside,
		"loop": root,
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("no symlinks here: %v", err)
		}
	}
	u := measureAll(t, root, entryLimit)
	if u.bytes == 0 || u.bytes >= 4<<20 || u.partial {
		t.Errorf("measured %d bytes, partial %v; want the small file only", u.bytes, u.partial)
	}

	// Storage that is itself a link is still measured where it points.
	link := filepath.Join(t.TempDir(), "storage")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if u := measureAll(t, link, entryLimit); u.bytes < 4<<20 {
		t.Errorf("through a linked root: %d bytes; want the 4 MiB file", u.bytes)
	}
}

func TestMeasure_EntryCapIsPartial(t *testing.T) {
	root := t.TempDir()
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if u := measureAll(t, root, 5); !u.partial {
		t.Error("a walk past the entry cap isn't partial")
	}
	if u := measureAll(t, root, 100); u.partial {
		t.Error("a walk within the cap is partial")
	}
}

func TestMeasure_TimeCapIsPartial(t *testing.T) {
	root := t.TempDir()
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A clock that has always already run out.
	start := time.Now()
	calls := 0
	clock := func() time.Time { calls++; return start.Add(time.Duration(calls) * time.Hour) }
	u, err := measure(context.Background(), root, time.Minute, entryLimit, clock)
	if err != nil || !u.partial {
		t.Errorf("past the time cap: %+v, %v; want partial", u, err)
	}
}

func TestMeasure_UnreadableDirIsSkipped(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	if err := os.MkdirAll(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "secret"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "open"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) }) // so TempDir can clean up; failing here changes nothing
	u, err := measure(context.Background(), root, time.Minute, entryLimit, time.Now)
	if err != nil {
		t.Fatalf("an unreadable subdirectory failed the walk: %v", err)
	}
	if u.bytes == 0 || u.bytes >= 1<<20 || !u.partial {
		t.Errorf("measured %d bytes, partial %v; want the readable file, marked partial", u.bytes, u.partial)
	}
}

func TestMeasure_MissingRootIsAnError(t *testing.T) {
	if _, err := measure(context.Background(), filepath.Join(t.TempDir(), "nope"), time.Minute, entryLimit, time.Now); err == nil {
		t.Error("a missing root measured")
	}
}

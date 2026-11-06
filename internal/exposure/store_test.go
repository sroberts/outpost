package exposure

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_SetGetResetAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exposure.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("tasking"); ok {
		t.Error("a new store has a setting")
	}
	if err := s.Set("tasking", true, []string{"/webhooks/github", "/hooks/*"}, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("yarr", false, nil, "root"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("tasking", true, []string{"/webhooks/github"}, "alice"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// It lasts: nothing else could rebuild it.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, ok := s.Get("tasking")
	if !ok || !got.Funnel || strings.Join(got.PublicPaths, ",") != "/webhooks/github" || got.UpdatedBy != "alice" {
		t.Errorf("tasking = %+v %v", got, ok)
	}
	// A setting of no paths is a setting, not the absence of one.
	if funnel, paths, ok := s.Override("yarr"); !ok || funnel || len(paths) != 0 {
		t.Errorf("yarr = %v %v %v", funnel, paths, ok)
	}
	if len(s.List()) != 2 {
		t.Errorf("list = %+v", s.List())
	}
	if err := s.Reset("tasking"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("tasking"); ok {
		t.Error("still set after a reset")
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v %v", fi.Mode(), err)
	}
}

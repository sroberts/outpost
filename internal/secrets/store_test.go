package secrets

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func must(t *testing.T, _ Secret, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolve_ServiceBindingBeatsGlobal(t *testing.T) {
	s, _ := openTest(t)
	sec, err := s.Create("openai", "OPENAI_API_KEY", "shared-key")
	must(t, sec, err)
	sec, err = s.Create("openai-yarr", "OPENAI_API_KEY", "yarr-key")
	must(t, sec, err)
	sec, err = s.Create("stripe", "STRIPE_KEY", "sk_live")
	must(t, sec, err)
	sec, err = s.Bind("openai", "")
	must(t, sec, err)
	sec, err = s.Bind("openai-yarr", "yarr")
	must(t, sec, err)
	sec, err = s.Bind("stripe", "tasking")
	must(t, sec, err)

	for _, tc := range []struct {
		service string
		want    map[string]string
	}{
		{"yarr", map[string]string{"OPENAI_API_KEY": "yarr-key"}},
		{"tasking", map[string]string{"OPENAI_API_KEY": "shared-key", "STRIPE_KEY": "sk_live"}},
		{"sous", map[string]string{"OPENAI_API_KEY": "shared-key"}},
	} {
		got, rev := s.Resolve(tc.service)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.service, got, tc.want)
		}
		for k, v := range tc.want {
			if got[k] != v {
				t.Errorf("%s: %s = %q, want %q", tc.service, k, got[k], v)
			}
		}
		if rev == "" {
			t.Errorf("%s: empty revision with secrets bound", tc.service)
		}
	}

	if got := s.Reaches("openai", []string{"sous", "tasking", "yarr"}); strings.Join(got, ",") != "sous,tasking" {
		t.Errorf("openai reaches %v, want sous and tasking (yarr has its own)", got)
	}
}

func TestResolve_NothingBound(t *testing.T) {
	s, _ := openTest(t)
	sec, err := s.Create("unused", "KEY", "v")
	must(t, sec, err)
	if vars, rev := s.Resolve("sous"); vars != nil || rev != "" {
		t.Errorf("got %v %q for a service with nothing bound", vars, rev)
	}
}

func TestRevision_ChangesOnRotateAndBindingButNotForOthers(t *testing.T) {
	s, _ := openTest(t)
	sec, err := s.Create("a", "A", "1")
	must(t, sec, err)
	sec, err = s.Create("b", "B", "1")
	must(t, sec, err)
	sec, err = s.Bind("a", "sous")
	must(t, sec, err)
	sec, err = s.Bind("b", "yarr")
	must(t, sec, err)

	_, sous0 := s.Resolve("sous")
	_, yarr0 := s.Resolve("yarr")

	sec, err = s.Rotate("a", "2")
	must(t, sec, err)
	if sec.Revision != 2 {
		t.Errorf("revision after rotate = %d, want 2", sec.Revision)
	}
	vars, sous1 := s.Resolve("sous")
	if sous1 == sous0 || vars["A"] != "2" {
		t.Errorf("rotating a: sous revision %q -> %q, A = %q", sous0, sous1, vars["A"])
	}
	if _, yarr1 := s.Resolve("yarr"); yarr1 != yarr0 {
		t.Error("rotating a changed yarr's revision, which would restart it for nothing")
	}

	sec, err = s.Bind("b", "sous")
	must(t, sec, err)
	if _, sous2 := s.Resolve("sous"); sous2 == sous1 {
		t.Error("binding b to sous didn't change sous's revision")
	}
	sec, err = s.Unbind("b", "sous")
	must(t, sec, err)
	if _, sous3 := s.Resolve("sous"); sous3 != sous1 {
		t.Error("unbinding b didn't bring sous's revision back")
	}
}

func TestBind_RefusesTwoAtTheSameLevel(t *testing.T) {
	s, _ := openTest(t)
	for _, n := range []string{"one", "two"} {
		sec, err := s.Create(n, "SAME", n)
		must(t, sec, err)
	}
	sec, err := s.Bind("one", "sous")
	must(t, sec, err)
	if _, err := s.Bind("two", "sous"); err == nil || !strings.Contains(err.Error(), "already gets SAME from secret one") {
		t.Errorf("second binding to sous: %v", err)
	}
	sec, err = s.Bind("one", "")
	must(t, sec, err)
	if _, err := s.Bind("two", ""); err == nil || !strings.Contains(err.Error(), "every service") {
		t.Errorf("second global binding: %v", err)
	}
	// Binding the same one again is fine.
	sec, err = s.Bind("one", "sous")
	must(t, sec, err)
}

func TestCreate_Validates(t *testing.T) {
	s, _ := openTest(t)
	for _, tc := range []struct{ name, env, value string }{
		{"Bad Name", "KEY", "v"},
		{"-lead", "KEY", "v"},
		{"ok", "1KEY", "v"},
		{"ok", "KEY-DASH", "v"},
		{"ok", "OUTPOST_PORT", "v"},
		{"ok", "outpost_storage_dir", "v"},
		{"ok", "PATH", "v"},
		{"ok", "KEY", ""},
		{"ok", "KEY", "a\x00b"},
		{"ok", "KEY", strings.Repeat("x", MaxValue+1)},
	} {
		if _, err := s.Create(tc.name, tc.env, tc.value); err == nil {
			t.Errorf("Create(%q, %q, %d bytes) succeeded", tc.name, tc.env, len(tc.value))
		}
	}
	sec, err := s.Create("ok", "KEY", "v")
	must(t, sec, err)
	if _, err := s.Create("ok", "OTHER", "w"); !errors.Is(err, ErrExists) {
		t.Errorf("duplicate create: %v", err)
	}
	if _, err := s.Rotate("missing", "v"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate missing: %v", err)
	}
	if _, err := s.Bind("missing", "sous"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bind missing: %v", err)
	}
	if _, err := s.Bind("ok", "bad\nname"); err == nil {
		t.Error("bound to a service name with a newline")
	}
}

func TestDelete_DropsBindings(t *testing.T) {
	s, _ := openTest(t)
	sec, err := s.Create("k", "K", "v")
	must(t, sec, err)
	sec, err = s.Bind("k", "sous")
	must(t, sec, err)
	before, err := s.Delete("k")
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Services) != 1 || before.Services[0] != "sous" {
		t.Errorf("Delete reported %+v, want it bound to sous", before)
	}
	if vars, _ := s.Resolve("sous"); len(vars) != 0 {
		t.Errorf("sous still gets %v", vars)
	}
	// Recreating the name starts without the old binding.
	sec, err = s.Create("k", "K", "v2")
	must(t, sec, err)
	if vars, _ := s.Resolve("sous"); len(vars) != 0 {
		t.Errorf("a recreated secret inherited a binding: %v", vars)
	}
	if _, err := s.Delete("k2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete missing: %v", err)
	}
}

func TestValues_SealedOnDiskAndReadBack(t *testing.T) {
	dir := t.TempDir()
	db, key := filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key")
	s, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	const value = "sk-very-recognisable-value-0123456789"
	sec, err := s.Create("k", "K", value)
	must(t, sec, err)
	sec, err = s.Bind("k", "")
	must(t, sec, err)
	s.Close()

	for _, f := range []string{db, db + "-wal"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(value)) {
			t.Errorf("%s holds the value in clear", filepath.Base(f))
		}
	}
	for _, f := range []string{db, key} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v, want 0600", filepath.Base(f), fi.Mode().Perm())
		}
	}

	s, err = Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if vars, _ := s.Resolve("any"); vars["K"] != value {
		t.Errorf("after reopening, K = %q", vars["K"])
	}
}

func TestOpen_RefusesMissingKeyWithSecrets(t *testing.T) {
	dir := t.TempDir()
	db, key := filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key")
	s, err := Open(db, key)
	if err != nil {
		t.Fatal(err)
	}
	sec, err := s.Create("k", "K", "v")
	must(t, sec, err)
	s.Close()

	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db, key); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("opened without its key: %v", err)
	}
	if _, err := os.Stat(key); !errors.Is(err, os.ErrNotExist) {
		t.Error("a new key was written over the lost one")
	}

	// The wrong key is refused too, rather than handing services garbage.
	if err := os.WriteFile(key, bytes.Repeat([]byte{7}, keySize), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(db, key); err == nil || !strings.Contains(err.Error(), "can't be decrypted") {
		t.Fatalf("opened with the wrong key: %v", err)
	}
}

func TestList_NeverCarriesValues(t *testing.T) {
	s, _ := openTest(t)
	sec, err := s.Create("k", "K", "the-value")
	must(t, sec, err)
	for _, sec := range s.List() {
		if strings.Contains(strings.Join([]string{sec.Name, sec.Env}, " "), "the-value") {
			t.Error("List leaked the value")
		}
	}
	got, _ := s.Get("k")
	got.Services = append(got.Services, "mutated")
	if again, _ := s.Get("k"); len(again.Services) != 0 {
		t.Error("Get handed out the cache's own slice")
	}
}

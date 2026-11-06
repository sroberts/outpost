package secrets

import (
	"path/filepath"
	"testing"
)

func TestValue_ReadsBackForTheDaemonOnly(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok := s.Value("openai-api-key"); ok {
		t.Error("a secret that doesn't exist has a value")
	}
	if _, err := s.Create("openai-api-key", DefaultEnv("openai-api-key"), "sk-1"); err != nil {
		t.Fatal(err)
	}
	if v, ok := s.Value("openai-api-key"); !ok || v != "sk-1" {
		t.Errorf("Value = %q, %v", v, ok)
	}
	if _, err := s.Rotate("openai-api-key", "sk-2"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Value("openai-api-key"); v != "sk-2" {
		t.Errorf("after Rotate, Value = %q", v)
	}
	// Created unbound, it reaches no service.
	if vars, _ := s.Resolve("sous"); len(vars) != 0 {
		t.Errorf("an unbound secret reached a service: %v", vars)
	}
}

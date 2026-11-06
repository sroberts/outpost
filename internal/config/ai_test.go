package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAI_Defaults(t *testing.T) {
	var a AI
	if a.ListenAddr() != "127.0.0.1:47901" || a.OllamaBase() != "http://127.0.0.1:11434" {
		t.Errorf("defaults = %s, %s", a.ListenAddr(), a.OllamaBase())
	}
	if (AI{OllamaURL: "http://box:11434/"}).OllamaBase() != "http://box:11434" {
		t.Error("OllamaBase kept the trailing slash")
	}
}

func TestAI_Validate(t *testing.T) {
	ok := []AI{
		{},
		{LLM: "gemma4:e4b", System1: "laya:latest"},
		{LLM: "hf.co/org/model:Q4_K_M"},
		{Listen: "localhost:9000", OllamaURL: "https://ollama.tail.ts.net"},
		{Listen: "[::1]:47901"},
	}
	for _, a := range ok {
		if err := a.validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	bad := map[string]AI{
		"loopback":   {Listen: "0.0.0.0:47901"},
		"host:port":  {Listen: "47901"},
		"fixed port": {Listen: "127.0.0.1:0"},
		"scheme":     {OllamaURL: "ftp://box"},
		"want a":     {OllamaURL: "http://"},
		"model name": {LLM: "gemma4 --verbose"},
		"system1":    {System1: "laya\nllm = \"x\""},
	}
	for want, a := range bad {
		err := a.validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v; want an error with %q", a, err, want)
		}
	}
}

func TestAI_LoadAndRenderRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[ai]\nllm = \"gemma4:e4b\"\nsystem1 = \"laya:latest\"\nollama_url = \"http://box:11434\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := AI{LLM: "gemma4:e4b", System1: "laya:latest", OllamaURL: "http://box:11434"}
	if cfg.AI != want {
		t.Fatalf("AI = %+v", cfg.AI)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.AI != want {
		t.Errorf("after Save, AI = %+v", again.AI)
	}
	if err := Save(path, Config{AI: AI{Listen: "0.0.0.0:1"}}); err == nil {
		t.Error("Save wrote an invalid [ai]")
	}
}

func TestAI_FingerprintIgnoresAnUnsetOrDefaultTable(t *testing.T) {
	base := Defaults()
	withDefaults := base
	withDefaults.AI = AI{Listen: DefaultAIListen, OllamaURL: DefaultOllamaURL + "/"}
	if base.Fingerprint() != withDefaults.Fingerprint() {
		t.Error("writing out [ai]'s defaults changed the fingerprint")
	}
	chosen := base
	chosen.AI.LLM = "gemma4:e4b"
	if base.Fingerprint() == chosen.Fingerprint() {
		t.Error("choosing an LLM in config.toml didn't change the fingerprint")
	}
}

package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
)

func TestClassify(t *testing.T) {
	cases := map[string]log.Level{
		"started echo (pid 42)":                         log.InfoLevel,
		"echo: healthcheck failed: connection refused":  log.ErrorLevel,
		"persist echo: database is locked":              log.InfoLevel,
		"echo: crash loop, giving up":                   log.ErrorLevel,
		"could not stop the schedule (expected if ...)": log.WarnLevel,
		"manifests not reloaded, previous set in force": log.WarnLevel,
		"reloaded manifests (3 service(s))":             log.InfoLevel,
		"tsnet: node echo: retrying in 4s":              log.WarnLevel,
	}
	for msg, want := range cases {
		if got := Classify(msg); got != want {
			t.Errorf("Classify(%q) = %v, want %v", msg, got, want)
		}
	}
}

func TestLogf_TextHasLevelPrefixAndNoOutpostTag(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "info", "text").WithPrefix("supervisor")
	Logf(l)("outpost: started %s", "echo")
	line := b.String()
	for _, want := range []string{"INFO", "supervisor", "started echo"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "outpost: ") {
		t.Errorf("line %q keeps the old outpost: tag", line)
	}
}

func TestLogf_JSONIsOneObjectPerLine(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "info", "json").WithPrefix("gateway")
	Logf(l)("listen failed: %s", "address in use")
	Logf(l)("up")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines:\n%s", len(lines), b.String())
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["level"] != "error" || rec["prefix"] != "gateway" || rec["msg"] != "listen failed: address in use" || rec["time"] == nil {
		t.Errorf("record = %v", rec)
	}
}

func TestNew_LevelFilters(t *testing.T) {
	var b bytes.Buffer
	Logf(New(&b, "warn", "logfmt"))("started echo")
	if b.Len() != 0 {
		t.Errorf("an info line got past a warn level: %q", b.String())
	}
	Logf(New(&b, "warn", "logfmt"))("could not reach it")
	if !strings.Contains(b.String(), "level=warn") {
		t.Errorf("logfmt line = %q", b.String())
	}
}

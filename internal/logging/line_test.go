package logging

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/log"
)

// A package handed a plain func gets each line's format and arguments as they
// were, whatever its level, so a test reading formats still reads them.
func TestLine_PlainFuncGetsTheFormatUntouched(t *testing.T) {
	var got []string
	logf := func(format string, v ...any) { got = append(got, format+"|"+fmt.Sprint(v...)) }
	l := New(&bytes.Buffer{}, "error", "text")
	To(l, logf, "service", "echo").Debugf("outpost: started %s", "echo")
	if len(got) != 1 || got[0] != "outpost: started %s|echo" {
		t.Errorf("logf got %q", got)
	}
}

func TestLine_LoggerGetsLevelFieldsAndNoOldTag(t *testing.T) {
	var b bytes.Buffer
	l := Component(New(&b, "info", "logfmt"), "gateway", "info")
	To(l, nil, "service", "yarr").Warnf("gateway: %s: proxy: %v", "yarr", "refused")
	line := b.String()
	for _, want := range []string{"level=warn", "prefix=gateway", `msg="yarr: proxy: refused"`, "service=yarr"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q lacks %q", line, want)
		}
	}
	b.Reset()
	To(l, nil).Debugf("hidden")
	if b.Len() != 0 {
		t.Errorf("a debug line got past info: %q", b.String())
	}
}

func TestLine_WithAddsWithoutSharing(t *testing.T) {
	var b bytes.Buffer
	l := New(&b, "info", "logfmt")
	base := To(l, nil, "a", 1)
	base.With("b", 2).Infof("one")
	base.Infof("two")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "b=2") || strings.Contains(lines[1], "b=2") {
		t.Errorf("lines = %q", lines)
	}
}

// Each part's level is its own: one turned up doesn't turn up the rest.
func TestComponent_LevelsAreIndependent(t *testing.T) {
	var b bytes.Buffer
	root := New(&b, "info", "logfmt")
	gw := Component(root, "gateway", "debug")
	sup := Component(root, "supervisor", "info")
	gw.Debug("from the gateway")
	sup.Debug("from the supervisor")
	if out := b.String(); !strings.Contains(out, "from the gateway") || strings.Contains(out, "from the supervisor") {
		t.Errorf("out = %q", out)
	}
}

func TestNewTee_BothGetEveryLineAndTheFileNoColour(t *testing.T) {
	var term, file bytes.Buffer
	l := NewTee(&term, &file, "info", "text")
	l.Info("hello", "service", "echo")
	if !strings.Contains(term.String(), "hello") || !strings.Contains(file.String(), "hello") {
		t.Fatalf("term %q, file %q", term.String(), file.String())
	}
	// Written straight to the tee, as a coloured line would arrive.
	if _, err := (tee{term: &term, file: &file}).Write([]byte("\x1b[1;31mERRO\x1b[0m boom\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(file.String(), "\x1b") || !strings.Contains(file.String(), "ERRO boom") {
		t.Errorf("file = %q", file.String())
	}
	if !strings.Contains(term.String(), "\x1b[1;31m") {
		t.Errorf("term lost its colour: %q", term.String())
	}
}

func TestWriter_OneLinePerEntry(t *testing.T) {
	var b bytes.Buffer
	w := Writer(New(&b, "info", "logfmt"), log.WarnLevel)
	fmt.Fprintf(w, "outpost: AI models: one\nAI models: two\n")
	lines := strings.Split(strings.TrimSpace(b.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `msg="AI models: one"`) || !strings.Contains(lines[1], "level=warn") {
		t.Errorf("lines = %q", lines)
	}
}

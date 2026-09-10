package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLog(t *testing.T, home, service, body string) {
	t.Helper()
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, service+".log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTailLog_ReturnsTheLastLines(t *testing.T) {
	home := newHome(t)
	a := &app{home: home, out: os.Stdout, err: os.Stderr}

	var b strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	writeLog(t, home, "demo", b.String())

	out, err := a.tailLog("demo", 5)
	if err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d lines; want 5:\n%s", len(lines), out)
	}
	if lines[4] != "line 50" || lines[0] != "line 46" {
		t.Errorf("wrong window: first=%q last=%q", lines[0], lines[4])
	}
}

// The dashboard re-reads this every couple of seconds and logs rotate at
// 10 MiB, so it must read a bounded tail rather than the whole file.
func TestTailLog_BoundedOnALargeFile(t *testing.T) {
	home := newHome(t)
	a := &app{home: home, out: os.Stdout, err: os.Stderr}

	var b strings.Builder
	for i := 0; b.Len() < 2*tailWindow; i++ {
		fmt.Fprintf(&b, "line %d padded with filler to make this file large\n", i)
	}
	full := b.String()
	writeLog(t, home, "big", full)

	out, err := a.tailLog("big", 3)
	if err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	if len(out) > tailWindow {
		t.Errorf("returned %d bytes from a %d-byte file; want a bounded tail", len(out), len(full))
	}

	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines; want 3", len(lines))
	}
	// The last line of the file must be the last line returned.
	wantLast := strings.Split(strings.TrimRight(full, "\n"), "\n")
	if lines[2] != wantLast[len(wantLast)-1] {
		t.Errorf("last line = %q; want %q", lines[2], wantLast[len(wantLast)-1])
	}
	// Reading from a byte offset lands mid-line; no fragment may survive.
	for i, line := range lines {
		if !strings.HasPrefix(line, "line ") {
			t.Errorf("line %d is a fragment: %q", i, line)
		}
	}
}

func TestTailLog_FewerLinesThanAsked(t *testing.T) {
	home := newHome(t)
	a := &app{home: home, out: os.Stdout, err: os.Stderr}
	writeLog(t, home, "small", "only one line\n")

	out, err := a.tailLog("small", 20)
	if err != nil {
		t.Fatalf("tailLog: %v", err)
	}
	if out != "only one line" {
		t.Errorf("tailLog = %q", out)
	}
}

func TestTailLog_MissingFileIsAnError(t *testing.T) {
	home := newHome(t)
	a := &app{home: home, out: os.Stdout, err: os.Stderr}
	if _, err := a.tailLog("nope", 5); err == nil {
		t.Fatal("tailLog on a missing file: want error, got nil")
	}
}

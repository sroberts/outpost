package applog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse_Formats(t *testing.T) {
	cases := []struct {
		line, level, msg string
		field            [2]string
	}{
		{`{"level":"error","msg":"db locked","table":"users"}`, LevelError, "db locked", [2]string{"table", "users"}},
		{`{"severity":"WARNING","message":"slow","ms":812}`, LevelWarn, "slow", [2]string{"ms", "812"}},
		{`time=2026-11-18T12:00:00Z level=info msg="listening on :8080" port=8080`, LevelInfo, "listening on :8080", [2]string{"port", "8080"}},
		{`ERROR could not open db`, LevelError, "ERROR could not open db", [2]string{}},
		{`2026/11/18 12:00:00 [warn] cache cold`, LevelWarn, "2026/11/18 12:00:00 [warn] cache cold", [2]string{}},
		{`2026-11-18 12:00:00 DEBUG tick`, LevelDebug, "2026-11-18 12:00:00 DEBUG tick", [2]string{}},
		{`error handling improved in this build`, LevelNone, "error handling improved in this build", [2]string{}},
		{`a=b c=d`, LevelNone, "a=b c=d", [2]string{}},
		{"\x1b[31mERROR\x1b[0m red alert", LevelError, "ERROR red alert", [2]string{}},
		{`{not json`, LevelNone, "{not json", [2]string{}},
	}
	for _, c := range cases {
		r := Parse(c.line)
		if r.Level != c.level || r.Message != c.msg {
			t.Errorf("Parse(%q) = level %q msg %q; want %q %q", c.line, r.Level, r.Message, c.level, c.msg)
		}
		if c.field[0] != "" && r.Fields[c.field[0]] != c.field[1] {
			t.Errorf("Parse(%q) fields = %v; want %s=%s", c.line, r.Fields, c.field[0], c.field[1])
		}
		if r.Line != c.line {
			t.Errorf("Parse(%q).Line = %q; want the line as written", c.line, r.Line)
		}
	}
}

func TestFormat_RoundTrips(t *testing.T) {
	at := time.Date(2026, 11, 18, 16, 16, 38, 123456000, time.FixedZone("x", 3600))
	b := Format(at, Stderr, `{"level":"warn","msg":"hi"}`)
	if !strings.HasPrefix(string(b), "2026-11-18T15:16:38.123456Z err ") || !strings.HasSuffix(string(b), "\n") {
		t.Fatalf("stored %q", b)
	}
	r := Parse(string(b))
	if !r.Time.Equal(at) || r.Stream != Stderr || r.Level != LevelWarn || r.Message != "hi" || r.Line != `{"level":"warn","msg":"hi"}` {
		t.Errorf("parsed back %+v", r)
	}
	// A line from before the prefix existed reads as it was.
	if r := Parse("plain old line"); !r.Time.IsZero() || r.Stream != "" || r.Line != "plain old line" {
		t.Errorf("legacy line parsed as %+v", r)
	}
}

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRead_NewestAcrossRotationsWithFilters(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }
	line := func(i int, stream, text string) string { return string(Format(at(i), stream, text)) }

	// Oldest in .2, then .1, then the live file.
	writeFile(t, filepath.Join(dir, "web.log.2"), "legacy unstamped\n", line(1, Stdout, "one"), line(2, Stderr, "ERROR two"))
	writeFile(t, filepath.Join(dir, "web.log.1"), line(3, Stdout, "three"), line(4, Stderr, `{"level":"warn","msg":"four"}`))
	writeFile(t, filepath.Join(dir, "web.log"), line(5, Stdout, "five"), line(6, Stdout, "level=error msg=six"))

	texts := func(rs []Record) string {
		var s []string
		for _, r := range rs {
			s = append(s, r.Message)
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		q    Query
		want string
	}{
		{Query{Lines: 3}, "four,five,six"},
		{Query{Lines: 100}, "legacy unstamped,one,ERROR two,three,four,five,six"},
		{Query{Level: "warn"}, "ERROR two,four,six"},
		{Query{Level: "error", Lines: 1}, "six"},
		{Query{Stream: Stderr}, "ERROR two,four"},
		{Query{Contains: "THRE"}, "three"},
		{Query{Since: at(4)}, "five,six"},
	}
	for _, c := range cases {
		got, err := Read(dir, "web", c.q)
		if err != nil {
			t.Fatal(err)
		}
		if texts(got) != c.want {
			t.Errorf("Read(%+v) = %s; want %s", c.q, texts(got), c.want)
		}
		for _, r := range got {
			if r.Service != "web" {
				t.Errorf("record without its service: %+v", r)
			}
		}
	}
}

func TestRead_ManyChunks(t *testing.T) {
	dir := t.TempDir()
	var b bytes.Buffer
	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	for i := 0; b.Len() < 5*chunk; i++ {
		b.Write(Format(base.Add(time.Duration(i)*time.Millisecond), Stdout, fmt.Sprintf("line %d %s", i, strings.Repeat("x", 100))))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.log"), b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	all, err := Read(dir, "big", Query{Lines: MaxLines})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(all), strings.Count(b.String(), "\n"); got != want {
		t.Fatalf("read %d lines of %d", got, want)
	}
	for i, r := range all {
		if !strings.HasPrefix(r.Message, fmt.Sprintf("line %d ", i)) {
			t.Fatalf("line %d is %q: lines split or out of order across chunks", i, r.Message[:20])
		}
	}
}

func TestRead_RefusesPathsOutOfTheDirectory(t *testing.T) {
	for _, name := range []string{"", ".", "..", "../etc/passwd", "a/b", `a\b`, "x\x00"} {
		if _, err := Read(t.TempDir(), name, Query{}); err == nil || err == ErrNoLog {
			t.Errorf("Read(%q) = %v; want refused", name, err)
		}
	}
	if _, err := Read(t.TempDir(), "absent", Query{}); err != ErrNoLog {
		t.Errorf("a service with no file: %v; want ErrNoLog", err)
	}
}

func TestReadAll_InterleavesByTime(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	at := func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }
	writeFile(t, filepath.Join(dir, "a.log"), string(Format(at(1), Stdout, "a1")), string(Format(at(4), Stdout, "a4")))
	writeFile(t, filepath.Join(dir, "b.log"), string(Format(at(2), Stdout, "b2")), string(Format(at(3), Stderr, "b3")))
	got, err := ReadAll(dir, []string{"a", "b", "nolog"}, Query{Lines: 3})
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, r := range got {
		s = append(s, r.Service+":"+r.Message)
	}
	if strings.Join(s, " ") != "b:b2 b:b3 a:a4" {
		t.Errorf("ReadAll = %v", s)
	}
}

func TestLineWriter_SplitsStampsAndFlushes(t *testing.T) {
	var out bytes.Buffer
	w := NewLineWriter(&out, Stderr)
	w.now = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	_, _ = w.Write([]byte("first\r\nsec"))
	_, _ = w.Write([]byte("ond\nunfinished"))
	if strings.Count(out.String(), "\n") != 2 {
		t.Fatalf("before flush: %q", out.String())
	}
	_ = w.Flush()
	want := "2026-01-02T03:04:05.000000Z err first\n" +
		"2026-01-02T03:04:05.000000Z err second\n" +
		"2026-01-02T03:04:05.000000Z err unfinished\n"
	if out.String() != want {
		t.Errorf("stored:\n%s\nwant:\n%s", out.String(), want)
	}

	out.Reset()
	_, _ = w.Write(bytes.Repeat([]byte("y"), MaxLineBytes+10))
	_ = w.Flush()
	if n := strings.Count(out.String(), "\n"); n != 2 {
		t.Errorf("an over-long line stored as %d lines; want it split in 2", n)
	}
}

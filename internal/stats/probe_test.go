package stats

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"
)

func TestProbeFor(t *testing.T) {
	if p, ok := ProbeFor("linux").(procfs); !ok || p.root != "/proc" {
		t.Errorf("linux = %#v; want procfs on /proc", ProbeFor("linux"))
	}
	if p, ok := ProbeFor("darwin").(psProbe); !ok || p.run == nil {
		t.Errorf("darwin = %#v; want ps", ProbeFor("darwin"))
	}
	if _, err := ProbeFor("plan9").Snapshot(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("plan9 snapshot err = %v; want ErrUnsupported", err)
	}
}

// statLine is a /proc/<pid>/stat line with the fields the probe reads set.
func statLine(pid int, comm string, pgrp int, utime, stime, rss uint64) string {
	return fmt.Sprintf("%d (%s) S 1 %d %d 0 -1 4194560 100 0 0 0 %d %d 0 0 20 0 1 0 12345 1000000 %d 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n",
		pid, comm, pgrp, pgrp, utime, stime, rss)
}

func TestProcfs(t *testing.T) {
	root := t.TempDir()
	write := func(pid int, body string) {
		dir := filepath.Join(root, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if body != "" {
			if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A service named to trip up anything that splits on the first ')' and
	// two children in its group.
	write(100, statLine(100, "my (weird) app", 100, 100, 50, 10))
	write(101, statLine(101, "worker", 100, 20, 30, 5))
	write(102, statLine(102, "worker) S 1 999", 100, 0, 0, 1))
	// Unrelated.
	write(200, statLine(200, "other", 200, 1000, 0, 7))
	// A group whose leader has gone.
	write(401, statLine(401, "orphan", 400, 1, 0, 1))
	// Exited between listing /proc and reading its stat.
	write(300, "")
	// Things in /proc that aren't processes.
	if err := os.MkdirAll(filepath.Join(root, "self"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("MemTotal: 1 kB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(500, "500 (truncated) S 1 500")

	groups, err := procfs{root: root}.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	page := uint64(os.Getpagesize())
	want := Group{CPUTime: 200 * 10 * time.Millisecond, RSS: 16 * page, Leader: true}
	if g := groups[100]; g != want {
		t.Errorf("group 100 = %+v; want %+v", g, want)
	}
	if g := groups[200]; g.CPUTime != 10*time.Second || g.RSS != 7*page || !g.Leader {
		t.Errorf("group 200 = %+v", g)
	}
	if g := groups[400]; g.Leader {
		t.Errorf("group 400 = %+v; its leader is gone", g)
	}
	if len(groups) != 3 {
		t.Errorf("groups = %v; want 100, 200 and 400 only", groups)
	}
}

func TestParsePSTime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"0:01.23":    1230 * time.Millisecond,
		"12:34.56":   12*time.Minute + 34560*time.Millisecond,
		"689:13.61":  689*time.Minute + 13610*time.Millisecond, // macOS doesn't roll minutes into hours
		"1:02:03.45": time.Hour + 2*time.Minute + 3450*time.Millisecond,
		"2-01:02:03": 49*time.Hour + 2*time.Minute + 3*time.Second,
		"00:00:00":   0,
	} {
		got, err := parsePSTime(in)
		if err != nil || got != want {
			t.Errorf("parsePSTime(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "12", "a:01.00", "1:-2.00", "1:2:3:4", "x-1:00", "1:02.3x", " 1:02"} {
		if got, err := parsePSTime(in); err == nil {
			t.Errorf("parsePSTime(%q) = %v; want an error", in, got)
		}
	}
}

func TestParsePS(t *testing.T) {
	out := []byte(`    1     1  26128 689:13.61
  100   100   1000   0:01.00
  101   100    500   1:00.50
  200   200      4 2-00:00:00
  401   400      8   0:00.10
  bogus line
  300   300   abc   0:00.00
  301   301    10   soon
  302          10   0:00.00

`)
	groups := parsePS(out)
	if g := groups[100]; g.CPUTime != 61500*time.Millisecond || g.RSS != 1500*1024 || !g.Leader {
		t.Errorf("group 100 = %+v", g)
	}
	if g := groups[200]; g.CPUTime != 48*time.Hour || g.RSS != 4096 {
		t.Errorf("group 200 = %+v", g)
	}
	if g := groups[400]; g.Leader {
		t.Errorf("group 400 = %+v; its leader isn't listed", g)
	}
	for _, pgid := range []int{300, 301, 302} {
		if _, ok := groups[pgid]; ok {
			t.Errorf("malformed line for %d was read", pgid)
		}
	}
}

func TestPSProbe_RunnerFailure(t *testing.T) {
	p := psProbe{run: func(context.Context) ([]byte, error) { return nil, errors.New("boom") }}
	if _, err := p.Snapshot(context.Background()); err == nil {
		t.Error("a failed ps gave a snapshot")
	}
}

// The probe for this platform finds a real process group and its leader.
func TestProbe_RealProcess(t *testing.T) {
	p := ProbeFor(goruntime.GOOS)
	if _, err := p.Snapshot(context.Background()); errors.Is(err, ErrUnsupported) {
		t.Skipf("no probe on %s", goruntime.GOOS)
	}
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Skipf("can't start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // the test is over; it only had to exist
		_ = cmd.Wait()
	})
	groups, err := p.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	g, ok := groups[cmd.Process.Pid]
	if !ok || !g.Leader || g.RSS == 0 {
		t.Errorf("group %d = %+v, %v; want a leader with memory", cmd.Process.Pid, g, ok)
	}
}

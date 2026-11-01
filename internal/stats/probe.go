package stats

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Group is what one process group is using. The runtime starts every service
// as the leader of its own group, so a group is a service and whatever it has
// spawned. A child that moves itself into another group or session (a
// daemonizing helper, say) is no longer counted: there is no cheap,
// cgo-free way to follow it, and the supervisor can't stop it either.
type Group struct {
	// CPUTime is the user and system time of the group's live processes. A
	// process that exits takes its time with it, so this can go down.
	CPUTime time.Duration
	RSS     uint64 // resident bytes, summed over the group
	// Leader is whether the process whose pid is the group's id is still
	// among them. Without it the pid may have been reused, or the service
	// has exited and left children behind.
	Leader bool
}

// Probe reads the CPU and memory of every process group on the machine at
// once, keyed by process group id. One snapshot per tick costs the same
// however many services there are.
type Probe interface {
	Snapshot(ctx context.Context) (map[int]Group, error)
}

// ErrUnsupported is a Probe's answer on a platform it can't read. Everything
// else the sampler measures still works.
var ErrUnsupported = errors.New("stats: no process probe for this platform")

// probeTimeout bounds one snapshot, so a wedged ps can't stall sampling.
const probeTimeout = 5 * time.Second

// ProbeFor returns the Probe for a GOOS value. Every implementation is built
// on every platform, as internal/autostart does it, so each can be tested
// from either.
func ProbeFor(goos string) Probe {
	switch goos {
	case "linux":
		return procfs{root: "/proc"}
	case "darwin":
		return psProbe{run: runPS}
	default:
		return unsupported{}
	}
}

type unsupported struct{}

func (unsupported) Snapshot(context.Context) (map[int]Group, error) { return nil, ErrUnsupported }

// procfs reads /proc/<pid>/stat for every process.
type procfs struct{ root string }

// userHZ is the unit of utime and stime in /proc/<pid>/stat. The kernel
// reports them in USER_HZ, which is 100 on every architecture Outpost builds
// for; asking sysconf(_SC_CLK_TCK) would need cgo.
const userHZ = 100

func (p procfs) Snapshot(ctx context.Context) (map[int]Group, error) {
	entries, err := os.ReadDir(p.root)
	if err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	groups := map[int]Group{}
	for i, e := range entries {
		if i%256 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue // not a process: /proc/self, /proc/meminfo and the like
		}
		b, err := os.ReadFile(filepath.Join(p.root, e.Name(), "stat"))
		if err != nil {
			continue // exited between the listing and the read
		}
		st, err := parseProcStat(b)
		if err != nil {
			continue
		}
		g := groups[st.pgrp]
		g.CPUTime += time.Duration(st.ticks) * (time.Second / userHZ)
		g.RSS += st.rssPages * page
		g.Leader = g.Leader || pid == st.pgrp
		groups[st.pgrp] = g
	}
	return groups, nil
}

type procStat struct {
	pgrp     int
	ticks    uint64 // utime + stime
	rssPages uint64
}

// parseProcStat reads the fields it needs from a /proc/<pid>/stat line. The
// command name comes second, in parentheses, and can hold anything, spaces
// and parentheses included, so fields are counted from the last ')'.
func parseProcStat(b []byte) (procStat, error) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return procStat{}, errors.New("stat: no command name")
	}
	// After the name, fields start at 3 (state): field n is f[n-3].
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 22 {
		return procStat{}, fmt.Errorf("stat: %d fields after the name", len(f))
	}
	pgrp, err1 := strconv.Atoi(f[5-3])
	utime, err2 := strconv.ParseUint(f[14-3], 10, 64)
	stime, err3 := strconv.ParseUint(f[15-3], 10, 64)
	rss, err4 := strconv.ParseInt(f[24-3], 10, 64)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return procStat{}, fmt.Errorf("stat: %w", err)
	}
	// A kernel thread reports 0; nothing reports less, but a negative here
	// must not wrap into an enormous unsigned number.
	return procStat{pgrp: pgrp, ticks: utime + stime, rssPages: uint64(max(rss, 0))}, nil
}

// psProbe asks ps, since macOS has no /proc and reading another process's
// usage directly would take cgo (proc_pidinfo).
type psProbe struct {
	run func(ctx context.Context) ([]byte, error)
}

// runPS is bounded by the caller's context, which the sampler gives
// probeTimeout.
func runPS(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=,pgid=,rss=,time=").Output()
}

func (p psProbe) Snapshot(ctx context.Context) (map[int]Group, error) {
	out, err := p.run(ctx)
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePS(out), nil
}

// parsePS reads `ps -o pid=,pgid=,rss=,time=`: rss in KiB, time as
// [[dd-]hh:]mm:ss.cc. A line it can't read is skipped rather than failing
// the snapshot, so one odd process can't blind the sampler to the rest.
func parsePS(out []byte) map[int]Group {
	groups := map[int]Group{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) != 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		pgid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseUint(f[2], 10, 64)
		cpu, err4 := parsePSTime(f[3])
		if errors.Join(err1, err2, err3, err4) != nil || pid <= 0 || pgid <= 0 {
			continue
		}
		g := groups[pgid]
		g.CPUTime += cpu
		g.RSS += rss * 1024
		g.Leader = g.Leader || pid == pgid
		groups[pgid] = g
	}
	return groups
}

// parsePSTime reads ps's cputime: [[dd-]hh:]mm:ss[.frac]. macOS lets the
// minutes run past 59 rather than show hours, so no field is range-checked.
func parsePSTime(s string) (time.Duration, error) {
	bad := fmt.Errorf("ps: bad time %q", s)
	var d time.Duration
	if days, rest, ok := strings.Cut(s, "-"); ok {
		n, err := digits(days)
		if err != nil {
			return 0, bad
		}
		d += time.Duration(n) * 24 * time.Hour
		s = rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, bad
	}
	secs, frac, _ := strings.Cut(parts[len(parts)-1], ".")
	n, err := digits(secs)
	if err != nil {
		return 0, bad
	}
	d += time.Duration(n) * time.Second
	if frac != "" {
		f, err := digits(frac)
		if err != nil || len(frac) > 9 {
			return 0, bad
		}
		for i := len(frac); i < 9; i++ {
			f *= 10
		}
		d += time.Duration(f)
	}
	unit := time.Minute
	for i := len(parts) - 2; i >= 0; i-- {
		n, err := digits(parts[i])
		if err != nil {
			return 0, bad
		}
		d += time.Duration(n) * unit
		unit = time.Hour
	}
	return d, nil
}

// digits parses a non-empty run of ASCII digits, refusing the signs and
// spaces strconv would otherwise accept or report differently.
func digits(s string) (int64, error) {
	if s == "" || len(s) > 12 {
		return 0, strconv.ErrSyntax
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, strconv.ErrSyntax
		}
	}
	return strconv.ParseInt(s, 10, 64)
}

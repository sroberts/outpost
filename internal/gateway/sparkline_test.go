package gateway

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var pathChars = regexp.MustCompile(`^[ML0-9 ,.]*$`)

// points reads a sparkPath back as its subpaths' y coordinates.
func points(t *testing.T, d string) [][]float64 {
	t.Helper()
	var out [][]float64
	for _, sub := range strings.Fields(d) {
		if !strings.HasPrefix(sub, "M") {
			t.Fatalf("subpath %q doesn't start with M", sub)
		}
		var ys []float64
		for _, p := range strings.Split(strings.ReplaceAll(sub[1:], "L", " "), " ") {
			_, y, ok := strings.Cut(p, ",")
			if !ok {
				t.Fatalf("bad point %q in %q", p, d)
			}
			v, err := strconv.ParseFloat(y, 64)
			if err != nil {
				t.Fatal(err)
			}
			ys = append(ys, v)
		}
		out = append(out, ys)
	}
	return out
}

func TestSparkPath_GapsStartNewSegments(t *testing.T) {
	nan := math.NaN()
	d := sparkPath([]float64{1, 2, nan, nan, 3, 4, nan, 5}, 70, 20)
	subs := points(t, d)
	if len(subs) != 3 {
		t.Fatalf("%q has %d subpaths; want 3", d, len(subs))
	}
	if len(subs[0]) != 2 || len(subs[1]) != 2 {
		t.Errorf("%q: want two-point runs either side of the first gap", d)
	}
	// The highest point touches the top margin, a lone one is still drawn.
	if subs[2][0] != 1 || len(subs[2]) != 2 {
		t.Errorf("lone last point drawn as %v; want a level stroke at the top", subs[2])
	}
}

func TestSparkPath_IdleIsAFlatLine(t *testing.T) {
	for _, series := range [][]float64{{0, 0, 0, 0}, {0.001, 0.002, 0, 0.001}} {
		d := sparkPathMin(series, 100, 24, 1)
		subs := points(t, d)
		if len(subs) != 1 {
			t.Fatalf("%q: want one line", d)
		}
		for _, y := range subs[0] {
			if math.Abs(y-23) > 0.1 {
				t.Errorf("%v drew %q; want a line along the bottom", series, d)
				break
			}
		}
	}
}

func TestSparkPath_SinglePoint(t *testing.T) {
	d := sparkPath([]float64{7}, 100, 20)
	if d != "M0,1L100,1" {
		t.Errorf("single point = %q; want a line across", d)
	}
	if d := sparkPath([]float64{math.NaN()}, 100, 20); d != "" {
		t.Errorf("no data = %q; want nothing", d)
	}
	if d := sparkPath(nil, 100, 20); d != "" {
		t.Errorf("empty = %q", d)
	}
}

func TestSparkPath_Charset(t *testing.T) {
	inf := math.Inf(1)
	for _, series := range [][]float64{
		{-5, 3, -1e300, 1e300, inf, -inf, math.NaN(), 0.000001},
		{1e-9, 2e-9},
		{3, 2, 1},
	} {
		d := sparkPath(series, 120, 16)
		if !pathChars.MatchString(d) {
			t.Errorf("%v drew %q, outside [ML0-9 ,.]", series, d)
		}
		if strings.Contains(d, "e") {
			t.Errorf("%q uses an exponent", d)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.0 KB",
		84_000_000:    "84 MB",
		84_400_000:    "84 MB",
		999_600:       "1.0 MB",
		1_200_000_000: "1.2 GB",
		9_960_000_000: "10 GB",
		1 << 62:       "4.6 EB",
	} {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q; want %q", n, got, want)
		}
	}
}

func TestCPUPercent(t *testing.T) {
	for cores, want := range map[float64]string{
		0:     "0.0%",
		0.031: "3.1%",
		0.5:   "50%",
		4:     "400%",
	} {
		if got := cpuPercent(cores); got != want {
			t.Errorf("cpuPercent(%v) = %q; want %q", cores, got, want)
		}
	}
	if got := cpuPercent(math.NaN()); got != "–" {
		t.Errorf("unknown = %q; want a dash", got)
	}
}

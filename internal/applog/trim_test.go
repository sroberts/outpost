package applog

import "testing"

func TestTrimLevel(t *testing.T) {
	cases := map[string]string{
		"ERROR could not open db":               "could not open db",
		"2026/11/18 12:00:00 [warn] cache cold": "cache cold",
		"WARN: slow":                            "slow",
		"INFO - listening":                      "listening",
		"ERROR":                                 "ERROR",
		"error handling improved":               "error handling improved",
		"plain line":                            "plain line",
	}
	for in, want := range cases {
		if got := TrimLevel(in); got != want {
			t.Errorf("TrimLevel(%q) = %q; want %q", in, got, want)
		}
	}
}

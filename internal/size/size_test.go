package size

import "testing"

func TestBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		0:             "0 B",
		999:           "999 B",
		1000:          "1.0 KB",
		999_499:       "999 KB",
		999_600:       "1.0 MB",
		84_400_000:    "84 MB",
		1_200_000_000: "1.2 GB",
		9_960_000_000: "10 GB",
		1 << 62:       "4.6 EB",
	} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q; want %q", n, got, want)
		}
	}
}

package autostart

import "testing"

// UnitBinary names the file `outpost update --self` replaces, so it has to read
// back whatever Render can write, spaces and markup included.
func TestUnitBinary_RoundTripsRender(t *testing.T) {
	binaries := []string{
		"/usr/local/bin/outpost",
		"/Users/me/My Tools/outpost",
		"/tmp/a&b<c>/outpost",
		"/tmp/50%/outpost",
		`/tmp/quo"te/outpost`,
	}
	for _, goos := range []string{"darwin", "linux"} {
		m, _ := For(goos)
		for _, binary := range binaries {
			cfg := baseConfig()
			cfg.Binary = binary
			body, err := m.Render(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.UnitBinary(body)
			if err != nil {
				t.Errorf("%s %q: %v", goos, binary, err)
			} else if got != binary {
				t.Errorf("%s: got %q; want %q", goos, got, binary)
			}
		}
	}
}

func TestUnitBinary_ReportsAFileItCantRead(t *testing.T) {
	for _, c := range []struct{ goos, unit string }{
		{"darwin", "<plist><dict><key>Label</key><string>x</string></dict></plist>"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><array></array></dict></plist>"},
		{"darwin", "<plist><dict><key>ProgramArguments</key><array><string>"},
		{"linux", "[Service]\n"},
		{"linux", "[Service]\nExecStart=\n"},
	} {
		m, _ := For(c.goos)
		if binary, err := m.UnitBinary(c.unit); err == nil {
			t.Errorf("%s %q: binary %q, no error", c.goos, c.unit, binary)
		}
	}
}

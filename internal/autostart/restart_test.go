package autostart

import (
	"reflect"
	"strings"
	"testing"
)

func TestRestartCommands(t *testing.T) {
	cases := []struct {
		goos  string
		scope Scope
		uid   int
		want  [][]string
	}{
		{"darwin", User, 501, [][]string{{"launchctl", "kickstart", "-k", "gui/501/" + DefaultLabel}}},
		{"darwin", System, 501, [][]string{{"sudo", "launchctl", "kickstart", "-k", "system/" + DefaultLabel}}},
		// Root may have no sudo to call at all (#19).
		{"darwin", System, 0, [][]string{{"launchctl", "kickstart", "-k", "system/" + DefaultLabel}}},
		{"linux", User, 1000, [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "restart", "outpost.service"},
		}},
		{"linux", System, 1000, [][]string{
			{"sudo", "systemctl", "daemon-reload"},
			{"sudo", "systemctl", "restart", "outpost.service"},
		}},
		{"linux", System, 0, [][]string{
			{"systemctl", "daemon-reload"},
			{"systemctl", "restart", "outpost.service"},
		}},
	}
	for _, c := range cases {
		m, err := For(c.goos)
		if err != nil {
			t.Fatal(err)
		}
		cfg := baseConfig()
		cfg.Scope, cfg.User = c.scope, "me"
		if got := m.RestartCommands(cfg, c.uid); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s/%s/uid %d: %v; want %v", c.goos, c.scope, c.uid, got, c.want)
		}
	}
}

// The probe runs on every install and restart, so it must never prompt.
func TestLoadedCommand_NeverNeedsSudo(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		m, _ := For(goos)
		for _, scope := range []Scope{User, System} {
			cfg := baseConfig()
			cfg.Scope, cfg.User = scope, "me"
			argv := m.LoadedCommand(cfg, 501)
			if len(argv) == 0 || argv[0] == "sudo" {
				t.Errorf("%s/%s: %v", goos, scope, argv)
			}
		}
	}
}

// launchd restarts what it loaded, not what is on disk, so only it needs
// separate commands to apply a rewritten file.
func TestReloadCommands(t *testing.T) {
	cfg := baseConfig()
	launchdM, _ := For("darwin")
	if got := launchdM.ReloadCommands("/p.plist", cfg, 501); len(got) != 2 ||
		strings.Join(got[0], " ") != "launchctl unload -w /p.plist" ||
		strings.Join(got[1], " ") != "launchctl load -w /p.plist" {
		t.Errorf("launchd: %v", got)
	}
	cfg.Scope, cfg.User = System, "me"
	if got := launchdM.ReloadCommands("/p.plist", cfg, 0); got[0][0] == "sudo" {
		t.Errorf("launchd as root: %v", got)
	}
	systemdM, _ := For("linux")
	if got := systemdM.ReloadCommands("/p", cfg, 501); len(got) != 0 {
		t.Errorf("systemd: %v", got)
	}
}

// UnitHome is what stops a restart reaching another home's daemon, so it has to
// read back whatever Render can write.
func TestUnitHome_RoundTripsRender(t *testing.T) {
	homes := []string{
		"/Users/me/.outpost",
		"/Users/me/My Outpost",
		"/tmp/a&b<c>",
		"/tmp/50%/x",
		`/tmp/quo"te`,
		`/tmp/back\slash`,
	}
	for _, goos := range []string{"darwin", "linux"} {
		m, _ := For(goos)
		for _, home := range homes {
			cfg := baseConfig()
			cfg.Home = home
			// An environment value mustn't be mistaken for the argument.
			cfg.Env = map[string]string{"DECOY": "--home /wrong"}
			body, err := m.Render(cfg)
			if err != nil {
				t.Fatal(err)
			}
			got, err := m.UnitHome(body)
			if err != nil {
				t.Errorf("%s %q: %v", goos, home, err)
			} else if got != home {
				t.Errorf("%s: got %q; want %q", goos, got, home)
			}
		}
	}
}

func TestUnitHome_ReportsAFileItCantRead(t *testing.T) {
	for _, c := range []struct{ goos, unit string }{
		{"darwin", "<plist><dict><key>ProgramArguments</key><array><string>/bin/x</string></array></dict></plist>"},
		{"darwin", "<plist><dict>"},
		{"linux", "[Service]\nExecStart=/bin/x serve\n"},
		{"linux", "[Service]\n"},
	} {
		m, _ := For(c.goos)
		if home, err := m.UnitHome(c.unit); err == nil {
			t.Errorf("%s %q: home %q, no error", c.goos, c.unit, home)
		}
	}
}

func TestLogHint(t *testing.T) {
	cfg := baseConfig()
	launchdM, _ := For("darwin")
	if got := launchdM.LogHint(cfg); got != "/Users/me/.outpost/logs/outpost.err.log" {
		t.Errorf("launchd: %q", got)
	}
	systemdM, _ := For("linux")
	if got := systemdM.LogHint(cfg); got != "journalctl --user -u outpost -e" {
		t.Errorf("systemd user: %q", got)
	}
	cfg.Scope, cfg.User = System, "me"
	if got := systemdM.LogHint(cfg); got != "journalctl -u outpost -e" {
		t.Errorf("systemd system: %q", got)
	}
}

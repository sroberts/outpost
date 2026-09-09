package autostart

import (
	"encoding/xml"
	"strings"
	"testing"
)

func baseConfig() Config {
	return Config{
		Label:  DefaultLabel,
		Binary: "/usr/local/bin/outpost",
		Home:   "/Users/me/.outpost",
	}
}

func render(t *testing.T, goos string, cfg Config) string {
	t.Helper()
	m, err := For(goos)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.Render(cfg)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

func TestFor_UnsupportedPlatform(t *testing.T) {
	if _, err := For("plan9"); err == nil {
		t.Fatal("For(plan9): want error, got nil")
	}
	for _, goos := range []string{"darwin", "linux"} {
		if _, err := For(goos); err != nil {
			t.Errorf("For(%s): %v", goos, err)
		}
	}
}

// The plist embeds user-chosen paths. Unescaped, "&" makes launchd reject the
// file outright and "</string>" could inject arbitrary keys.
func TestLaunchd_EscapesXMLMetacharacters(t *testing.T) {
	cfg := baseConfig()
	cfg.Home = "/Users/me/dev&test/.outpost"
	out := render(t, "darwin", cfg)

	if strings.Contains(out, "dev&test") {
		t.Error("raw & left in the plist; launchd will reject it as malformed XML")
	}
	if !strings.Contains(out, "dev&amp;test") {
		t.Errorf("& not escaped:\n%s", out)
	}
	// The whole document must parse as XML.
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Errorf("plist is not well-formed XML: %v", err)
	}
}

func TestLaunchd_ResistsPlistInjection(t *testing.T) {
	cfg := baseConfig()
	cfg.Home = `/tmp/x</string><key>ProgramArguments</key><array><string>/bin/sh`
	out := render(t, "darwin", cfg)

	// The injected markup must appear escaped, never as live elements.
	if strings.Count(out, "<key>ProgramArguments</key>") != 1 {
		t.Errorf("injected key became live markup:\n%s", out)
	}
	if strings.Contains(out, "<string>/bin/sh</string>") {
		t.Error("injected argument became live markup")
	}
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Errorf("plist is not well-formed XML: %v", err)
	}
}

func TestLaunchd_KeepsDaemonAlive(t *testing.T) {
	out := render(t, "darwin", baseConfig())
	for _, want := range []string{
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<key>ThrottleInterval</key>",
		"<key>ExitTimeOut</key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %s:\n%s", want, out)
		}
	}
}

func TestLaunchd_ScopePaths(t *testing.T) {
	m, _ := For("darwin")

	sys := baseConfig()
	sys.Scope, sys.User = System, "me"
	path, err := m.UnitPath(sys)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/Library/LaunchDaemons/") {
		t.Errorf("system scope path = %q; want a LaunchDaemon", path)
	}
	if out := render(t, "darwin", sys); !strings.Contains(out, "<key>UserName</key>") {
		t.Error("a LaunchDaemon must name the user it runs as, or it runs as root")
	}

	user, err := m.UnitPath(baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(user, "LaunchAgents") {
		t.Errorf("user scope path = %q; want a LaunchAgent", user)
	}
}

func TestSystemd_UnitContents(t *testing.T) {
	out := render(t, "linux", baseConfig())
	for _, want := range []string{
		"Type=simple",
		"Restart=always",
		"RestartSec=5",
		"KillMode=mixed",
		"TimeoutStopSec=60",
		"After=network-online.target",
		"ExecStart=/usr/local/bin/outpost --home /Users/me/.outpost serve",
		"WantedBy=default.target",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unit missing %q:\n%s", want, out)
		}
	}
}

func TestSystemd_SystemScopeRunsAsUser(t *testing.T) {
	cfg := baseConfig()
	cfg.Scope, cfg.User = System, "scott"
	out := render(t, "linux", cfg)

	if !strings.Contains(out, "User=scott") {
		t.Error("a system unit must name the user it runs as")
	}
	if !strings.Contains(out, "WantedBy=multi-user.target") {
		t.Error("a system unit must be wanted by multi-user.target to start at boot")
	}
}

// ExecStart is split on whitespace, so a path with a space must be quoted or it
// silently becomes two arguments.
func TestSystemd_QuotesPathsWithSpaces(t *testing.T) {
	cfg := baseConfig()
	cfg.Home = "/home/me/My Outpost"
	out := render(t, "linux", cfg)

	if !strings.Contains(out, `--home "/home/me/My Outpost" serve`) {
		t.Errorf("path with a space not quoted:\n%s", out)
	}
}

// % introduces a specifier in unit files and must be doubled to stay literal.
func TestSystemd_EscapesPercent(t *testing.T) {
	cfg := baseConfig()
	cfg.Home = "/home/me/100% outpost"
	out := render(t, "linux", cfg)

	if strings.Contains(out, "100% outpost") {
		t.Errorf("raw %% left in the unit; systemd will expand it as a specifier:\n%s", out)
	}
	if !strings.Contains(out, "100%% outpost") {
		t.Errorf("%% not doubled:\n%s", out)
	}
}

func TestSystemd_UnitNameFromLabel(t *testing.T) {
	for label, want := range map[string]string{
		"com.sroberts.outpost": "outpost.service",
		"outpost":              "outpost.service",
		"com.example.thing":    "thing.service",
	} {
		if got := unitName(label); got != want {
			t.Errorf("unitName(%q) = %q; want %q", label, got, want)
		}
	}
}

func TestSystemd_UserUnitPathHonoursXDG(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	m, _ := For("linux")
	path, err := m.UnitPath(baseConfig())
	if err != nil {
		t.Fatal(err)
	}
	if path != "/custom/config/systemd/user/outpost.service" {
		t.Errorf("path = %q; want it under XDG_CONFIG_HOME", path)
	}
}

func TestEnv_RenderedAndDeterministic(t *testing.T) {
	cfg := baseConfig()
	cfg.Env = map[string]string{"TS_AUTHKEY": "tskey-abc", "LOG_LEVEL": "debug"}

	linux := render(t, "linux", cfg)
	if !strings.Contains(linux, "Environment=LOG_LEVEL=debug") ||
		!strings.Contains(linux, "Environment=TS_AUTHKEY=tskey-abc") {
		t.Errorf("env missing from unit:\n%s", linux)
	}
	// Ordered, so regenerating produces an identical file.
	if strings.Index(linux, "LOG_LEVEL") > strings.Index(linux, "TS_AUTHKEY") {
		t.Error("env not emitted in sorted order; files will churn between runs")
	}

	mac := render(t, "darwin", cfg)
	if !strings.Contains(mac, "<key>EnvironmentVariables</key>") ||
		!strings.Contains(mac, "<string>tskey-abc</string>") {
		t.Errorf("env missing from plist:\n%s", mac)
	}
}

func TestRender_IsDeterministic(t *testing.T) {
	cfg := baseConfig()
	cfg.Env = map[string]string{"A": "1", "B": "2", "C": "3"}
	for _, goos := range []string{"darwin", "linux"} {
		first := render(t, goos, cfg)
		for i := 0; i < 5; i++ {
			if again := render(t, goos, cfg); again != first {
				t.Fatalf("%s: render is not deterministic", goos)
			}
		}
	}
}

func TestValidate_RejectsBadConfig(t *testing.T) {
	tests := map[string]func(*Config){
		"no label":            func(c *Config) { c.Label = "" },
		"no binary":           func(c *Config) { c.Binary = "" },
		"relative binary":     func(c *Config) { c.Binary = "outpost" },
		"no home":             func(c *Config) { c.Home = "" },
		"relative home":       func(c *Config) { c.Home = ".outpost" },
		"system without user": func(c *Config) { c.Scope = System },
	}
	for name, mutate := range tests {
		cfg := baseConfig()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestCommands_AreOfferedForBothScopes(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		m, _ := For(goos)
		for _, scope := range []Scope{User, System} {
			cfg := baseConfig()
			cfg.Scope, cfg.User = scope, "me"
			path, err := m.UnitPath(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.EnableCommands(path, cfg)) == 0 {
				t.Errorf("%s/%s: no enable commands", goos, scope)
			}
			if len(m.DisableCommands(path, cfg)) == 0 {
				t.Errorf("%s/%s: no disable commands", goos, scope)
			}
			if len(m.Notes(cfg)) == 0 {
				t.Errorf("%s/%s: no notes", goos, scope)
			}
		}
	}
}

// A system-scope install needs root; the commands must say so.
func TestSystemScope_CommandsUseSudo(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		m, _ := For(goos)
		cfg := baseConfig()
		cfg.Scope, cfg.User = System, "me"
		path, _ := m.UnitPath(cfg)

		found := false
		for _, cmd := range m.EnableCommands(path, cfg) {
			if cmd[0] == "sudo" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: system scope enable commands do not use sudo", goos)
		}
	}
}

// StartLimit* belong in [Unit]. systemd 229 moved them there; in [Service]
// they are ignored with a warning, so the rate limit would silently apply and
// systemd would eventually stop restarting the daemon.
func TestSystemd_StartLimitIsInUnitSection(t *testing.T) {
	out := render(t, "linux", baseConfig())

	unitSection, serviceSection, _ := strings.Cut(out, "[Service]")
	if !strings.Contains(unitSection, "StartLimitIntervalSec=0") {
		t.Errorf("StartLimitIntervalSec missing from [Unit]:\n%s", out)
	}
	if strings.Contains(serviceSection, "StartLimitIntervalSec") {
		t.Errorf("StartLimitIntervalSec is in [Service], where systemd ignores it:\n%s", out)
	}
}

// Section ordering must be valid: [Unit], then [Service], then [Install].
func TestSystemd_SectionOrder(t *testing.T) {
	out := render(t, "linux", baseConfig())
	unit := strings.Index(out, "[Unit]")
	service := strings.Index(out, "[Service]")
	install := strings.Index(out, "[Install]")
	if unit < 0 || service < 0 || install < 0 {
		t.Fatalf("missing a section:\n%s", out)
	}
	if !(unit < service && service < install) {
		t.Errorf("sections out of order: Unit=%d Service=%d Install=%d", unit, service, install)
	}
}

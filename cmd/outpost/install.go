package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"
)

// launchdLabel identifies the agent in launchctl.
const launchdLabel = "com.sroberts.outpost"

// plistTemplate keeps the Outpost daemon alive across logins and reboots.
// launchd supervises only Outpost itself; Outpost supervises the services (§3).
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>

	<key>ProgramArguments</key>
	<array>
		<string>{{.Binary}}</string>
		<string>--home</string>
		<string>{{.Home}}</string>
		<string>serve</string>
	</array>

	<key>RunAtLoad</key>
	<true/>

	<key>KeepAlive</key>
	<true/>

	<key>WorkingDirectory</key>
	<string>{{.Home}}</string>

	<key>StandardOutPath</key>
	<string>{{.Home}}/logs/outpost.out.log</string>

	<key>StandardErrorPath</key>
	<string>{{.Home}}/logs/outpost.err.log</string>

	<key>ProcessType</key>
	<string>Background</string>
</dict>
</plist>
`

// cmdInstall writes the launchd agent that keeps the daemon running.
//
// It does not load the agent unless asked: registering a background service on
// someone's machine is their call, not a side effect of generating a file.
func (a *app) cmdInstall(args []string) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	fs.SetOutput(a.err)
	load := fs.Bool("load", false, "load the agent with launchctl after writing it")
	printOnly := fs.Bool("print", false, "write nothing; print the plist to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("`outpost install` writes a launchd agent and only applies on macOS (this is %s); on Linux, run `outpost serve` under systemd", runtime.GOOS)
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate the outpost binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return err
	}
	var body strings.Builder
	if err := tmpl.Execute(&body, struct{ Label, Binary, Home string }{launchdLabel, binary, a.home}); err != nil {
		return err
	}

	if *printOnly {
		fmt.Fprint(a.out, body.String())
		return nil
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	plistPath, err := a.plistPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create LaunchAgents dir: %w", err)
	}
	if err := os.WriteFile(plistPath, []byte(body.String()), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}
	fmt.Fprintf(a.out, "wrote %s\n", plistPath)

	if !*load {
		fmt.Fprintf(a.out, "\nTo start it now and at every login:\n  launchctl load -w %s\n", plistPath)
		fmt.Fprintf(a.out, "To stop and unregister it:\n  launchctl unload -w %s\n", plistPath)
		return nil
	}

	cmd := exec.Command("launchctl", "load", "-w", plistPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load: %w\n%s", err, out)
	}
	fmt.Fprintf(a.out, "loaded %s\n", launchdLabel)
	return nil
}

// plistPath is where the user's LaunchAgents live.
func (a *app) plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

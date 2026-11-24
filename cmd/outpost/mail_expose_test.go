package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/mail"
	"github.com/sroberts/outpost/internal/supervisor"
)

func TestWriteMailStatus(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	var b bytes.Buffer
	o := ipc.MailOverview{
		Account: mail.Status{Address: "me@gmail.com", HasPassword: true, Checked: now.Add(-90 * time.Second), Routed: 4},
		Secret:  "outpost-mail-password",
		Apps: []ipc.MailApp{
			{Name: "sous", Send: true, Held: 2, Routes: []mail.Route{
				{Route: "sous", Address: "me+sous@gmail.com"}, {Route: "k3x9", Alias: true, Note: "receipts", Address: "me+k3x9@gmail.com"}}},
			{Name: "wiki"},
		},
	}
	if err := writeMailStatus(&b, o, now); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"account   me@gmail.com", "checked 1m30s ago; 4 routed", "outpost-mail-password",
		"me+sous@gmail.com, me+k3x9@gmail.com (receipts)", "wiki"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "app pass") {
		t.Error("a password reached the output")
	}

	b.Reset()
	_ = writeMailStatus(&b, ipc.MailOverview{}, now)
	if !strings.Contains(b.String(), "not set up") {
		t.Errorf("unset account:\n%s", b.String())
	}
}

func TestDescribeReach(t *testing.T) {
	s := supervisor.ServiceStatus{Name: "sous", PublicPaths: []string{"/public/*"}, Funnel: true,
		Tailnet: &supervisor.TailnetStatus{Funnel: supervisor.FunnelOn}, MCPPath: "/mcp", MCPFunnel: true,
		FromPortal: true, ManifestPublicPaths: nil}
	out := describeReach(s)
	for _, want := range []string{"public    /public/*", "funnel    on (on)", "set over the manifest, which has Funnel off", "/mcp on the internet"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
	if out := describeReach(supervisor.ServiceStatus{Name: "wiki"}); !strings.Contains(out, "only the tailnet") || strings.Contains(out, "mcp") {
		t.Errorf("tailnet only:\n%s", out)
	}
}

// Flags that contradict each other are refused before the daemon is asked.
func TestExpose_ConflictingFlags(t *testing.T) {
	a := &app{home: t.TempDir(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	for _, args := range [][]string{
		{"sous", "--reset", "--path", "/x"},
		{"sous", "--mcp", "on", "--funnel"},
		{"sous", "--mcp", "maybe"},
		{},
	} {
		if err := a.cmdExpose(args); err == nil {
			t.Errorf("expose %v went ahead", args)
		}
	}
}

func TestMail_Usage(t *testing.T) {
	a := &app{home: t.TempDir(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	for _, args := range [][]string{{"send", "sous"}, {"send", "sous", "maybe"}, {"token"}, {"alias", "rm", "sous"}, {"bogus"}} {
		if err := a.cmdMail(args); err == nil {
			t.Errorf("mail %v went ahead", args)
		}
	}
}

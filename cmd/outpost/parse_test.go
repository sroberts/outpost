package main

import (
	"flag"
	"io"
	"testing"
)

func statusFlags() (*flag.FlagSet, *int) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limit := fs.Int("events", 10, "events")
	return fs, limit
}

// Flags must work on either side of the service name.
func TestParseServiceCommand_FlagBeforeService(t *testing.T) {
	fs, limit := statusFlags()
	name, err := parseServiceCommand(fs, "status", []string{"--events", "20", "web"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if name != "web" {
		t.Errorf("service = %q; want %q", name, "web")
	}
	if *limit != 20 {
		t.Errorf("events = %d; want 20", *limit)
	}
}

func TestParseServiceCommand_FlagAfterService(t *testing.T) {
	fs, limit := statusFlags()
	name, err := parseServiceCommand(fs, "status", []string{"web", "--events", "20"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if name != "web" || *limit != 20 {
		t.Errorf("service = %q events = %d; want web and 20", name, *limit)
	}
}

func TestParseServiceCommand_EqualsForm(t *testing.T) {
	fs, limit := statusFlags()
	name, err := parseServiceCommand(fs, "status", []string{"--events=5", "web"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if name != "web" || *limit != 5 {
		t.Errorf("service = %q events = %d; want web and 5", name, *limit)
	}
}

func TestParseServiceCommand_BoolFlagEitherSide(t *testing.T) {
	for _, args := range [][]string{{"-f", "web"}, {"web", "-f"}} {
		fs := flag.NewFlagSet("logs", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		follow := fs.Bool("f", false, "follow")
		name, err := parseServiceCommand(fs, "logs", args)
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if name != "web" || !*follow {
			t.Errorf("%v: service = %q follow = %v; want web and true", args, name, *follow)
		}
	}
}

func TestParseServiceCommand_MissingServiceName(t *testing.T) {
	fs, _ := statusFlags()
	if _, err := parseServiceCommand(fs, "status", []string{"--events", "20"}); err == nil {
		t.Fatal("no service name: want error, got nil")
	}
}

func TestParseServiceCommand_RejectsSecondService(t *testing.T) {
	fs, _ := statusFlags()
	_, err := parseServiceCommand(fs, "status", []string{"web", "api"})
	if err == nil {
		t.Fatal("two service names: want error, got nil")
	}
}

func TestParseServiceCommand_UnknownFlagIsAnError(t *testing.T) {
	fs, _ := statusFlags()
	if _, err := parseServiceCommand(fs, "status", []string{"--nope", "web"}); err == nil {
		t.Fatal("unknown flag: want error, got nil")
	}
}

// restore takes an optional timestamp after the service name.
func TestParseOptionalService_ServiceAndTimestamp(t *testing.T) {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	got, err := parseOptionalService(fs, "restore", []string{"web", "2026-09-08T23-04-11"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || got[0] != "web" || got[1] != "2026-09-08T23-04-11" {
		t.Errorf("got %v; want [web 2026-09-08T23-04-11]", got)
	}
}

func TestParseOptionalService_NoArgsMeansAll(t *testing.T) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	got, err := parseOptionalService(fs, "backup", nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v; want none", got)
	}
}

func TestParseOptionalService_FlagWithService(t *testing.T) {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	list := fs.Bool("list", false, "list")
	got, err := parseOptionalService(fs, "backup", []string{"--list", "web"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*list || len(got) != 1 || got[0] != "web" {
		t.Errorf("list = %v got = %v; want true and [web]", *list, got)
	}
}

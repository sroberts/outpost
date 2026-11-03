package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBindArgs(t *testing.T) {
	for _, tc := range []struct {
		args          []string
		name, service string
		ok            bool
	}{
		{[]string{"k", "sous"}, "k", "sous", true},
		{[]string{"k", "--global"}, "k", "", true},
		{[]string{"--global", "k"}, "k", "", true},
		{[]string{"k"}, "", "", false},
		{[]string{"k", "sous", "yarr"}, "", "", false},
		{[]string{"k", "--force"}, "", "", false},
	} {
		name, service, err := bindArgs("bind", tc.args)
		if (err == nil) != tc.ok || name != tc.name || service != tc.service {
			t.Errorf("bindArgs(%v) = %q, %q, %v", tc.args, name, service, err)
		}
	}
}

// A piped value is taken whole, less the newline echo or a here-string adds,
// so a multi-line key arrives intact.
func TestSecretValue_FromStdin(t *testing.T) {
	a := &app{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	withStdin(t, "-----BEGIN KEY-----\nabc\n-----END KEY-----\n")
	v, err := a.secretValue("k", true)
	if err != nil {
		t.Fatal(err)
	}
	if v != "-----BEGIN KEY-----\nabc\n-----END KEY-----" {
		t.Errorf("value = %q", v)
	}

	withStdin(t, "")
	if _, err := a.secretValue("k", true); err == nil {
		t.Error("an empty stdin was taken as a value")
	}
	withStdin(t, "piped")
	if _, err := a.secretValue("k", false); err == nil || !strings.Contains(err.Error(), "--value-stdin") {
		t.Errorf("piped without --value-stdin: %v", err)
	}
}

// At a terminal it's asked for once, hidden, and the prompt goes to stderr.
func TestSecretValue_Prompted(t *testing.T) {
	errs := &bytes.Buffer{}
	a := &app{out: &bytes.Buffer{}, err: errs}
	asked := withTerminal(t, "sk-typed")
	v, err := a.secretValue("openai-key", false)
	if err != nil || v != "sk-typed" || *asked != 1 {
		t.Errorf("value %q, err %v, asked %d times", v, err, *asked)
	}
	if !strings.Contains(errs.String(), "Value for openai-key") {
		t.Errorf("prompt = %q", errs.String())
	}
}

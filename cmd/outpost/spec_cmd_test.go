package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sroberts/outpost"
)

func TestSpec_PrintsTheEmbeddedContract(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--home", t.TempDir(), "spec"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if out.String() != outpost.ServiceSpec() {
		t.Error("output isn't the embedded contract")
	}
}

func TestSpec_RevisionAlone(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run([]string{"--home", t.TempDir(), "spec", "--revision"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != outpost.ContractRevision() {
		t.Errorf("got %q; want %q", got, outpost.ContractRevision())
	}
}

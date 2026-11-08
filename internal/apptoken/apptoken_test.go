package apptoken

import (
	"strings"
	"testing"
)

func TestMint_StableAndDistinct(t *testing.T) {
	key := []byte("k")
	a := Mint("oai_", key, "sous", 0)
	if a != Mint("oai_", key, "sous", 0) || !strings.HasPrefix(a, "oai_") {
		t.Fatalf("not stable: %q", a)
	}
	for _, other := range []string{
		Mint("oai_", key, "sous", 1),
		Mint("oai_", key, "tasking", 0),
		Mint("oai_", []byte("j"), "sous", 0),
	} {
		if other == a {
			t.Errorf("collides: %q", other)
		}
	}
	// The separator keeps "sous"+"10" apart from "sous1"+"0".
	if Mint("oai_", key, "sous", 10) == Mint("oai_", key, "sous1", 0) {
		t.Error("name and generation run together")
	}
}

func TestIssued(t *testing.T) {
	i := NewIssued("omail_")
	tok := Mint("omail_", []byte("k"), "sous", 0)
	if _, ok := i.Service(tok); ok {
		t.Fatal("known before it was added")
	}
	i.Add(tok, "sous")
	if svc, ok := i.Service(tok); !ok || svc != "sous" {
		t.Fatalf("got %q %v", svc, ok)
	}
	if _, ok := i.Service("oai_" + strings.TrimPrefix(tok, "omail_")); ok {
		t.Error("another service's prefix was taken")
	}
	i.Remove(tok)
	if _, ok := i.Service(tok); ok {
		t.Error("still known after removal")
	}
}

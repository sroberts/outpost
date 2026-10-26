package auth

import (
	"strings"
	"testing"
)

func TestValidUsername(t *testing.T) {
	cases := []struct {
		name string
		ok   bool
	}{
		{"alice", true},
		{"a", true},
		{"0day", true},
		{"alice.smith", true},
		{"alice_smith-2", true},
		{strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false},
		{"", false},
		{"Alice", false},
		{".alice", false},
		{"-alice", false},
		{"_alice", false},
		{"alice smith", false},
		{"alice@example", false},
		{"alicé", false},
		{"ａlice", false},
		{"alice\n", false},
	}
	for _, c := range cases {
		err := ValidUsername(c.name)
		if (err == nil) != c.ok {
			t.Errorf("ValidUsername(%q) = %v; want ok=%v", c.name, err, c.ok)
		}
		if err := ValidGroup(c.name); (err == nil) != c.ok {
			t.Errorf("ValidGroup(%q) = %v; want ok=%v", c.name, err, c.ok)
		}
	}
}

func TestValidPassword(t *testing.T) {
	cases := []struct {
		pw string
		ok bool
	}{
		{"", false},
		{strings.Repeat("x", MinPasswordLen-1), false},
		{strings.Repeat("x", MinPasswordLen), true},
		{"correct horse battery", true},
		// Length is counted in characters, so twelve multi-byte runes pass
		// and eleven don't, however many bytes they take.
		{strings.Repeat("é", MinPasswordLen), true},
		{strings.Repeat("é", MinPasswordLen-1), false},
		{strings.Repeat("x", maxPasswordLen+1), false},
	}
	for _, c := range cases {
		if err := ValidPassword(c.pw); (err == nil) != c.ok {
			t.Errorf("ValidPassword(%d bytes) = %v; want ok=%v", len(c.pw), err, c.ok)
		}
	}
}

func TestValidText_RejectsControlCharacters(t *testing.T) {
	for _, s := range []string{"Alice\r\nX-Outpost-User: root", "tab\there", "nul\x00"} {
		if err := validText("display name", s); err == nil {
			t.Errorf("validText(%q) = nil; want an error", s)
		}
	}
	if err := validText("display name", "Ålice Exämple"); err != nil {
		t.Errorf("validText rejected an ordinary non-ASCII name: %v", err)
	}
}

package auth

import (
	"encoding/base64"
	"testing"
)

func TestNewToken(t *testing.T) {
	seen := map[string]bool{}
	for range 20 {
		tok, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if len(tok) != 43 {
			t.Errorf("token %q is %d characters; want 43 (32 bytes, unpadded base64url)", tok, len(tok))
		}
		raw, err := base64.RawURLEncoding.DecodeString(tok)
		if err != nil || len(raw) != 32 {
			t.Errorf("token %q is not 32 bytes of base64url: %v", tok, err)
		}
		if seen[tok] {
			t.Fatalf("token %q issued twice", tok)
		}
		seen[tok] = true
	}
}

func TestHashToken(t *testing.T) {
	tok, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	h := HashToken(tok)
	if h != HashToken(tok) {
		t.Error("HashToken is not deterministic")
	}
	if h == tok {
		t.Error("HashToken returned its input")
	}
	if len(h) != 64 {
		t.Errorf("HashToken = %q; want 64 hex characters", h)
	}
}

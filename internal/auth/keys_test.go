package auth

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

func TestJWKS_PublicKeyVerifiesSignature(t *testing.T) {
	k, err := GenerateKey(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := JWKS([]SigningKey{k})
	if err != nil {
		t.Fatal(err)
	}

	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("JWKS is not JSON: %v\n%s", err, body)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("JWKS has %d keys; want 1", len(set.Keys))
	}
	jwk := set.Keys[0]
	for field, want := range map[string]string{"kty": "OKP", "crv": "Ed25519", "alg": "EdDSA", "use": "sig", "kid": k.ID} {
		if jwk[field] != want {
			t.Errorf("jwk %s = %q; want %q", field, jwk[field], want)
		}
	}
	if _, ok := jwk["d"]; ok {
		t.Fatal("the JWKS publishes the private key")
	}

	x, err := base64.RawURLEncoding.DecodeString(jwk["x"])
	if err != nil || len(x) != ed25519.PublicKeySize {
		t.Fatalf("jwk x = %q is not an Ed25519 public key: %v", jwk["x"], err)
	}
	msg := []byte("header.payload")
	if !ed25519.Verify(ed25519.PublicKey(x), msg, ed25519.Sign(k.Private, msg)) {
		t.Error("the published key doesn't verify a signature made with the private key")
	}
}

func TestGenerateKey_IDIsDerivedFromKey(t *testing.T) {
	a, err := GenerateKey(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateKey(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.ID) != 16 {
		t.Errorf("kid %q is %d characters; want 16", a.ID, len(a.ID))
	}
	if a.ID == b.ID {
		t.Error("two keys got the same kid")
	}
	if a.ID != keyID(a.Public()) {
		t.Error("kid is not derived from the public key")
	}
}

package push

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

// RFC 8291 Appendix A works one message through by hand. Reproducing it byte
// for byte is what shows the key schedule is the standard one, not merely
// one this package agrees with itself about.
func TestEncrypt_MatchesRFC8291Example(t *testing.T) {
	plaintext := []byte("When I grow up, I want to be a watermelon")
	as, err := ecdh.P256().NewPrivateKey(b64(t, "yfWPiYE-n46HLnH0KqZOF1fJJU3MYrct3AELtAQ-oRw"))
	if err != nil {
		t.Fatal(err)
	}
	uaPublic := b64(t, "BCVxsr7N_eNgVRqvHtD0zTZsEc6-VV-JvLexhqUzORcxaOzi6-AYWXvTBHm4bjyPjs7Vd8pZGH6SRpkNtoIAiw4")
	authSecret := b64(t, "BTBZMqHH6r4Tts7J_aSIgg")
	salt := b64(t, "DGv6ra1nlYgDCS1FRnbzlw")

	got, err := encryptWith(plaintext, uaPublic, authSecret, as, salt)
	if err != nil {
		t.Fatal(err)
	}
	want := "DGv6ra1nlYgDCS1FRnbzlwAAEABBBP4z9KsN6nGRTbVYI_c7VJSPQTBtkgcy27mlmlMoZIIgDll6e3vCYLocInmYWAmS6TlzAC8wEqKK6PBru3jl7A_yl95bQpu6cVPTpK4Mqgkf1CXztLVBSt2Ks3oZwbuwXPXLWyouBWLVWGNWQexSgSxsj_Qulcy4a-fN"
	if g := base64.RawURLEncoding.EncodeToString(got); g != want {
		t.Errorf("body\n got %s\nwant %s", g, want)
	}
}

func TestEncrypt_RefusesWhatWontFitOneRecord(t *testing.T) {
	ua, _ := ecdh.P256().GenerateKey(nil)
	if _, err := encrypt(make([]byte, MaxPayload+1), ua.PublicKey().Bytes(), make([]byte, 16)); err == nil {
		t.Fatal("a payload past one record was sealed")
	}
	body, err := encrypt(make([]byte, MaxPayload), ua.PublicKey().Bytes(), make([]byte, 16))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != recordSize {
		t.Errorf("the largest payload sealed to %d bytes, want exactly %d", len(body), recordSize)
	}
}

// A push service checks the token against k=, for the endpoint's origin, and
// refuses an expiry more than a day out.
func TestVAPID_TokenVerifiesForTheEndpointsOrigin(t *testing.T) {
	k, err := newVAPIDKey()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := k.raw()
	if err != nil {
		t.Fatal(err)
	}
	k2, err := parseVAPIDKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if k2.public() != k.public() {
		t.Fatal("the key didn't survive being kept")
	}

	now := time.Unix(1_800_000_000, 0)
	ep, _ := url.Parse("https://web.push.apple.com/QGuQyavXutnMp6x2EAUnqMg?x=1")
	h, err := k2.authorization(ep, "mailto:ops@example.com", now)
	if err != nil {
		t.Fatal(err)
	}
	rest, ok := strings.CutPrefix(h, "vapid t=")
	if !ok {
		t.Fatalf("header %q", h)
	}
	jwt, pub, ok := strings.Cut(rest, ", k=")
	if !ok || pub != k.public() {
		t.Fatalf("header %q doesn't carry the public key", h)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("token %q", jwt)
	}
	var claims struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(b64(t, parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Aud != "https://web.push.apple.com" || claims.Sub != "mailto:ops@example.com" {
		t.Errorf("claims %+v", claims)
	}
	if d := time.Unix(claims.Exp, 0).Sub(now); d <= 0 || d > 24*time.Hour {
		t.Errorf("expires %v out; a push service takes at most a day", d)
	}

	pubKey, err := ecdsa.ParseUncompressedPublicKey(k.priv.Curve, b64(t, pub))
	if err != nil {
		t.Fatal(err)
	}
	sig := b64(t, parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature is %d bytes, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pubKey, digest[:], r, s) {
		t.Error("the signature doesn't verify against k=")
	}
}

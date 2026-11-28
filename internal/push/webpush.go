package push

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"
)

// Web Push is done here with the standard library rather than a third-party
// package: the whole of it is one ECDH, three HKDFs, one AES-GCM seal and one
// ES256 signature, each checked against the RFCs' own examples in the tests,
// and it keeps the binary free of another dependency to audit.

// recordSize is the aes128gcm record size declared in the header. One record
// carries the whole message, so any value at least as big as it will do;
// 4096 is what every push service accepts.
const recordSize = 4096

// headerLen is the aes128gcm header: salt, record size, key id length and a
// P-256 public key as the key id.
const headerLen = 16 + 4 + 1 + 65

// MaxPayload is the largest plaintext that still fits one 4096-byte record
// once the header, the delimiter and the GCM tag are added, which is the
// most a push service has to take (RFC 8030 §7.2).
const MaxPayload = recordSize - headerLen - 1 - 16

// encrypt seals plaintext for a subscriber as RFC 8291 describes: the body
// of the POST to its endpoint, with Content-Encoding aes128gcm.
func encrypt(plaintext, p256dh, authSecret []byte) ([]byte, error) {
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	return encryptWith(plaintext, p256dh, authSecret, as, salt)
}

// encryptWith is encrypt with the sender's key and the salt given, so the
// RFC's worked example can be reproduced byte for byte.
func encryptWith(plaintext, p256dh, authSecret []byte, as *ecdh.PrivateKey, salt []byte) ([]byte, error) {
	if len(plaintext) > MaxPayload {
		return nil, fmt.Errorf("the payload is %d bytes; at most %d fit", len(plaintext), MaxPayload)
	}
	if len(authSecret) != 16 {
		return nil, errors.New("the subscription's auth secret isn't 16 bytes")
	}
	ua, err := ecdh.P256().NewPublicKey(p256dh)
	if err != nil {
		return nil, fmt.Errorf("the subscription's p256dh key: %w", err)
	}
	shared, err := as.ECDH(ua)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()

	// RFC 8291 §3.3: the input keying material mixes the ECDH secret with
	// the subscriber's auth secret and both public keys.
	prkKey, err := hkdf.Extract(sha256.New, shared, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := "WebPush: info\x00" + string(p256dh) + string(asPub)
	ikm, err := hkdf.Expand(sha256.New, prkKey, keyInfo, 32)
	if err != nil {
		return nil, err
	}
	// RFC 8188 §2.2 and §2.3: the content key and nonce from it.
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, headerLen+len(plaintext)+1+gcm.Overhead())
	out = append(out, salt...)
	out = binary.BigEndian.AppendUint32(out, recordSize)
	out = append(out, byte(len(asPub)))
	out = append(out, asPub...)
	// 0x02 marks the last (and only) record, with no padding after it.
	record := append(append(make([]byte, 0, len(plaintext)+1), plaintext...), 0x02)
	return gcm.Seal(out, nonce, record, nil), nil
}

// vapidKey is the application server's key pair (RFC 8292), the one every
// subscription is made against.
type vapidKey struct {
	priv *ecdsa.PrivateKey
}

func newVAPIDKey() (vapidKey, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return vapidKey{}, err
	}
	return vapidKey{priv: k}, nil
}

// parseVAPIDKey reads a private key as raw kept it.
func parseVAPIDKey(b []byte) (vapidKey, error) {
	k, err := ecdsa.ParseRawPrivateKey(elliptic.P256(), b)
	if err != nil {
		return vapidKey{}, err
	}
	return vapidKey{priv: k}, nil
}

// raw is the private key as push.db keeps it.
func (k vapidKey) raw() ([]byte, error) { return k.priv.Bytes() }

// public is the uncompressed public key, base64url without padding: the
// applicationServerKey a browser subscribes with, and the k= of each request.
func (k vapidKey) public() string {
	b, err := k.priv.PublicKey.Bytes()
	if err != nil {
		return "" // can't happen: the key was made or parsed on P-256
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// vapidLife is how long a signed token is good for. RFC 8292 allows at most
// a day; a push service rejects one older than that.
const vapidLife = 12 * time.Hour

// authorization is the Authorization header for a request to endpoint:
// a JWT for the endpoint's origin signed with the key, and the public key.
func (k vapidKey) authorization(endpoint *url.URL, subject string, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"typ":"JWT","alg":"ES256"}`))
	claims, err := json.Marshal(struct {
		Aud string `json:"aud"`
		Exp int64  `json:"exp"`
		Sub string `json:"sub"`
	}{endpoint.Scheme + "://" + endpoint.Host, now.Add(vapidLife).Unix(), subject})
	if err != nil {
		return "", err
	}
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// JWS wants the two halves side by side, 32 bytes each, not ASN.1.
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + base64.RawURLEncoding.EncodeToString(sig) + ", k=" + k.public(), nil
}

// decodeKey reads a subscription key the way browsers write them: base64url,
// usually without padding, occasionally with it or in the standard alphabet.
func decodeKey(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	return nil, errors.New("not base64")
}

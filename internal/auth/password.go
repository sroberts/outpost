package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are argon2id's cost settings. They're stored in every hash, so they
// can be raised later without invalidating existing passwords.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32
	Threads uint8
	SaltLen uint32
	KeyLen  uint32
}

// DefaultParams are the settings new hashes use.
var DefaultParams = Params{Memory: 64 * 1024, Time: 3, Threads: 1, SaltLen: 16, KeyLen: 32}

// Ceilings on what a stored hash may ask for. A hash is only as trustworthy
// as the row it came from, and without these a tampered row could make a
// single login allocate any amount of memory.
const (
	maxMemory  = 1 << 20 // KiB, so 1 GiB
	maxTime    = 10
	maxSaltLen = 64
	maxKeyLen  = 64
	minSaltLen = 8
	minKeyLen  = 16
)

var errMalformedHash = errors.New("malformed password hash")

func (p Params) check() error {
	switch {
	case p.Memory == 0 || p.Memory > maxMemory:
		return fmt.Errorf("argon2 memory %d KiB is outside 1..%d", p.Memory, maxMemory)
	case p.Time == 0 || p.Time > maxTime:
		return fmt.Errorf("argon2 time %d is outside 1..%d", p.Time, maxTime)
	case p.Threads == 0:
		return errors.New("argon2 threads must be at least 1")
	case p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("argon2 salt length %d is outside %d..%d", p.SaltLen, minSaltLen, maxSaltLen)
	case p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("argon2 key length %d is outside %d..%d", p.KeyLen, minKeyLen, maxKeyLen)
	}
	return nil
}

var b64 = base64.RawStdEncoding

// HashPassword hashes pw with p, in PHC string format:
// $argon2id$v=19$m=65536,t=3,p=1$<salt>$<key>.
func HashPassword(pw string, p Params) (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	salt := make([]byte, p.SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	key := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// decodeHash parses a PHC string back into its parameters, salt and key.
func decodeHash(encoded string) (Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return Params{}, nil, nil, errMalformedHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return Params{}, nil, nil, errMalformedHash
	}
	var p Params
	// Sscanf ignores trailing input, so round-trip to reject "p=1junk".
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Time, &p.Threads); err != nil ||
		fmt.Sprintf("m=%d,t=%d,p=%d", p.Memory, p.Time, p.Threads) != parts[3] {
		return Params{}, nil, nil, errMalformedHash
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return Params{}, nil, nil, errMalformedHash
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return Params{}, nil, nil, errMalformedHash
	}
	p.SaltLen, p.KeyLen = uint32(len(salt)), uint32(len(key))
	if err := p.check(); err != nil {
		return Params{}, nil, nil, fmt.Errorf("%w: %v", errMalformedHash, err)
	}
	return p, salt, key, nil
}

// VerifyPassword reports whether pw matches encoded. A wrong password is
// (false, nil); an error means the stored hash itself is unusable.
func VerifyPassword(pw, encoded string) (bool, error) {
	p, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(pw), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was made with settings other than p,
// so a successful login can upgrade it.
func NeedsRehash(encoded string, p Params) bool {
	got, _, _, err := decodeHash(encoded)
	return err != nil || got != p
}

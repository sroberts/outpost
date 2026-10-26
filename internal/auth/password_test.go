package auth

import (
	"strings"
	"testing"
)

// testParams keep the suite fast. They're far below what DefaultParams costs,
// but every code path is the same.
var testParams = Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}

func TestHashPassword_VerifyRoundTrip(t *testing.T) {
	h, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Errorf("hash %q is not in the expected PHC form", h)
	}
	ok, err := VerifyPassword("correct horse battery", h)
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(right) = %v, %v; want true, nil", ok, err)
	}
}

func TestVerifyPassword_WrongPasswordIsFalseNotError(t *testing.T) {
	h, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := VerifyPassword("incorrect horse battery", h)
	if ok || err != nil {
		t.Errorf("VerifyPassword(wrong) = %v, %v; want false, nil", ok, err)
	}
}

func TestVerifyPassword_MalformedHashIsError(t *testing.T) {
	good, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	for _, bad := range []string{
		"",
		"plaintext",
		"$argon2i$v=19$m=64,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=16$m=64,t=1,p=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1junk$" + parts[4] + "$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1$!!!$" + parts[5],
		"$argon2id$v=19$m=64,t=1,p=1$" + parts[4],
	} {
		if ok, err := VerifyPassword("correct horse battery", bad); err == nil || ok {
			t.Errorf("VerifyPassword(%q) = %v, %v; want false and an error", bad, ok, err)
		}
	}
}

func TestVerifyPassword_RefusesAbsurdParameters(t *testing.T) {
	good, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(good, "$")
	for _, params := range []string{
		"m=2097152,t=1,p=1", // 2 GiB
		"m=64,t=1000,p=1",
		"m=0,t=1,p=1",
		"m=64,t=0,p=1",
		"m=64,t=1,p=0",
		"m=99999999999,t=1,p=1", // overflows uint32
	} {
		tampered := "$argon2id$v=19$" + params + "$" + parts[4] + "$" + parts[5]
		if _, err := VerifyPassword("correct horse battery", tampered); err == nil {
			t.Errorf("VerifyPassword accepted %s", params)
		}
	}
	if _, err := HashPassword("correct horse battery", Params{Memory: 1 << 30, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}); err == nil {
		t.Error("HashPassword accepted 1 TiB of memory")
	}
}

func TestNeedsRehash(t *testing.T) {
	h, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if NeedsRehash(h, testParams) {
		t.Error("NeedsRehash = true for a hash made with the same parameters")
	}
	stronger := testParams
	stronger.Time = 2
	if !NeedsRehash(h, stronger) {
		t.Error("NeedsRehash = false after the time cost changed")
	}
	longer := testParams
	longer.KeyLen = 48
	if !NeedsRehash(h, longer) {
		t.Error("NeedsRehash = false after the key length changed")
	}
	if !NeedsRehash("garbage", testParams) {
		t.Error("NeedsRehash = false for a malformed hash")
	}
}

func TestHashPassword_Salted(t *testing.T) {
	a, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashPassword("correct horse battery", testParams)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Error("the same password hashed to the same string twice; the salt isn't random")
	}
}

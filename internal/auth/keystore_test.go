package auth

import (
	"testing"
	"time"
)

func TestKeystore_FirstCurrentKeyCreatesOne(t *testing.T) {
	s, _ := newTestStore(t)
	k, err := s.CurrentKey()
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.CurrentKey()
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != again.ID || !k.Private.Equal(again.Private) {
		t.Error("a second CurrentKey made a new key instead of returning the first")
	}
	if !again.RetiredAt.IsZero() {
		t.Error("the current key is marked retired")
	}
}

func TestKeystore_RotationGracePeriod(t *testing.T) {
	s, c := newTestStore(t)
	old, err := s.CurrentKey()
	if err != nil {
		t.Fatal(err)
	}

	c.advance(time.Hour)
	fresh, err := s.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == old.ID {
		t.Fatal("rotation kept the same key")
	}
	if cur, _ := s.CurrentKey(); cur.ID != fresh.ID {
		t.Errorf("CurrentKey = %s after rotation; want %s", cur.ID, fresh.ID)
	}
	assertPublished(t, s, fresh.ID, old.ID)

	c.advance(KeyGracePeriod - time.Second)
	assertPublished(t, s, fresh.ID, old.ID)

	c.advance(time.Second)
	assertPublished(t, s, fresh.ID)

	// The next rotation deletes it outright.
	newest, err := s.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM signing_keys WHERE kid = ?`, old.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a key retired past the grace period was not pruned")
	}
	assertPublished(t, s, newest.ID, fresh.ID)
}

func TestKeystore_RotateWithNoKeyYet(t *testing.T) {
	s, _ := newTestStore(t)
	k, err := s.RotateKey()
	if err != nil {
		t.Fatal(err)
	}
	assertPublished(t, s, k.ID)
}

func assertPublished(t *testing.T, s *Store, want ...string) {
	t.Helper()
	keys, err := s.PublishedKeys()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range keys {
		got = append(got, k.ID)
	}
	if len(got) != len(want) {
		t.Fatalf("published %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("published %v; want %v (current first)", got, want)
		}
	}
	if _, err := JWKS(keys); err != nil {
		t.Errorf("published keys don't render: %v", err)
	}
}

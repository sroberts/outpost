package auth

import (
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// KeyGracePeriod is how long a retired key stays in the JWKS. Identity tokens
// live for a minute, so a day is far more than any token needs; the slack is
// for apps that cache the key set.
const KeyGracePeriod = 24 * time.Hour

const keyCols = `kid, private, created_at, retired_at`

func scanKey(sc scanner) (SigningKey, error) {
	var (
		k                SigningKey
		priv             []byte
		created, retired int64
	)
	if err := sc.Scan(&k.ID, &priv, &created, &retired); err != nil {
		return SigningKey{}, err
	}
	if len(priv) != ed25519.PrivateKeySize {
		return SigningKey{}, fmt.Errorf("signing key %s is malformed", k.ID)
	}
	k.Private = ed25519.PrivateKey(priv)
	k.CreatedAt = fromUnix(created)
	k.RetiredAt = fromUnix(retired)
	return k, nil
}

// CurrentKey returns the key new tokens are signed with, generating the first
// one on demand.
func (s *Store) CurrentKey() (SigningKey, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return SigningKey{}, err
	}
	defer rollback(tx)

	k, err := scanKey(tx.QueryRow(`SELECT ` + keyCols + ` FROM signing_keys WHERE retired_at = 0 ORDER BY created_at DESC LIMIT 1`))
	if err == nil {
		return k, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return SigningKey{}, fmt.Errorf("load signing key: %w", err)
	}
	k, err = insertKey(tx, s.now())
	if err != nil {
		return SigningKey{}, err
	}
	return k, tx.Commit()
}

// PublishedKeys returns the keys an app should accept: the current one, then
// any retired within KeyGracePeriod, newest first.
func (s *Store) PublishedKeys() ([]SigningKey, error) {
	if _, err := s.CurrentKey(); err != nil {
		return nil, err
	}
	cutoff := s.now().Add(-KeyGracePeriod).Unix()
	rows, err := s.db.Query(`SELECT `+keyCols+` FROM signing_keys
WHERE retired_at = 0 OR retired_at > ?
ORDER BY retired_at = 0 DESC, created_at DESC`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("load signing keys: %w", err)
	}
	defer rows.Close()
	var keys []SigningKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RotateKey retires the current key, makes a new one current, and deletes keys
// retired longer ago than KeyGracePeriod.
func (s *Store) RotateKey() (SigningKey, error) {
	now := s.now()
	tx, err := s.db.Begin()
	if err != nil {
		return SigningKey{}, err
	}
	defer rollback(tx)

	if _, err := tx.Exec(`UPDATE signing_keys SET retired_at = ? WHERE retired_at = 0`, now.Unix()); err != nil {
		return SigningKey{}, fmt.Errorf("retire signing key: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM signing_keys WHERE retired_at != 0 AND retired_at <= ?`,
		now.Add(-KeyGracePeriod).Unix()); err != nil {
		return SigningKey{}, fmt.Errorf("prune signing keys: %w", err)
	}
	k, err := insertKey(tx, now)
	if err != nil {
		return SigningKey{}, err
	}
	return k, tx.Commit()
}

func insertKey(tx *sql.Tx, now time.Time) (SigningKey, error) {
	k, err := GenerateKey(now)
	if err != nil {
		return SigningKey{}, err
	}
	if _, err := tx.Exec(`INSERT INTO signing_keys (kid, private, created_at) VALUES (?, ?, ?)`,
		k.ID, []byte(k.Private), now.Unix()); err != nil {
		return SigningKey{}, fmt.Errorf("store signing key: %w", err)
	}
	// Match what a later load returns, which has second precision.
	k.CreatedAt = fromUnix(now.Unix())
	return k, nil
}

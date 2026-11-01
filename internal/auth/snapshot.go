package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
)

// Snapshot copies the auth database at src to dst, a new file readable only by
// its owner, for `outpost update --self` to keep before a new daemon starts.
//
// This is the one place the CLI opens auth.db itself rather than asking the
// daemon, and it opens it read-only. The copy has to be taken before the new
// daemon gets to migrate the file, and a daemon from v0.1.0, the one being
// updated from, has nothing on its socket that could take it. VACUUM INTO
// gives a consistent copy of a database another process has open, the WAL
// included, which copying the file would not.
func Snapshot(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		return err
	}
	// Created empty first, owner-only and exclusively: VACUUM INTO accepts an
	// empty file, and would otherwise create one with the umask's mode, which
	// leaves password hashes readable by others until the chmod below.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%s already exists", dst)
		}
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dst) // an empty file is no snapshot
		return err
	}
	if err := vacuumInto(src, dst); err != nil {
		_ = os.Remove(dst) // a partial copy must not pass for a snapshot
		return err
	}
	// Belt and braces: SQLite may have replaced the file rather than written
	// into it.
	if err := os.Chmod(dst, 0o600); err != nil {
		return fmt.Errorf("chmod %s: %w", dst, err)
	}
	return nil
}

func vacuumInto(src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+src+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = db.Close() }() // read-only; closing loses nothing
	if _, err := db.Exec(`VACUUM INTO ?`, dst); err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	return nil
}

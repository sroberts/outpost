package stats

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// walkLimit and entryLimit bound one walk, so a storage directory holding
	// millions of small files can't keep the walker busy for good. A walk
	// that hits either reports what it had counted, marked partial.
	walkLimit  = 60 * time.Second
	entryLimit = 2_000_000
)

// usage is the result of one walk.
type usage struct {
	bytes   uint64
	partial bool // a lower bound: the walk stopped early or skipped something
}

// measure adds up the space a directory tree takes on disk, as du would:
// allocated blocks, each hard-linked file once, and symbolic links never
// followed, so a link to / can't make the walk the whole disk.
func measure(ctx context.Context, root string, limit time.Duration, maxEntries int, now func() time.Time) (usage, error) {
	// The root alone is resolved: storage the user moved elsewhere and left
	// a link to is still the service's storage.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return usage{}, err
	}
	start := now()
	var u usage
	entries := 0
	type inode struct{ dev, ino uint64 }
	linked := map[inode]bool{}
	err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == resolved {
				return err
			}
			// Unreadable, or gone since its directory was listed: skip it,
			// and say the total is short.
			u.partial = true
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		entries++
		if entries > maxEntries {
			u.partial = true
			return fs.SkipAll
		}
		if entries%1024 == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if now().Sub(start) > limit {
				u.partial = true
				return fs.SkipAll
			}
		}
		info, err := d.Info()
		if err != nil {
			u.partial = true
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			if st.Nlink > 1 && !info.IsDir() {
				k := inode{uint64(st.Dev), st.Ino}
				if linked[k] {
					return nil
				}
				linked[k] = true
			}
			u.bytes += uint64(max(st.Blocks, 0)) * 512
			return nil
		}
		// No block count: the apparent size is the best there is.
		u.bytes += uint64(max(info.Size(), 0))
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return usage{}, err
	}
	return u, nil
}

// Package backup archives a service's storage directory and restores it (§10).
//
// Archives are plain tar.gz, so they can be inspected and unpacked with
// standard tools rather than only by Outpost.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// TimestampFormat names archives; it is filename-safe and sorts lexically.
const TimestampFormat = "2006-01-02T15-04-05"

const archiveExt = ".tar.gz"

// Archive is one stored backup.
type Archive struct {
	Service   string    `json:"service"`
	Timestamp string    `json:"timestamp"`
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

// ArchivePath is where a service's backup for a given time lives:
// <root>/<service>/<timestamp>.tar.gz
func ArchivePath(root, service string, at time.Time) string {
	return filepath.Join(root, service, at.UTC().Format(TimestampFormat)+archiveExt)
}

// Create writes storageDir to destPath as a gzipped tarball.
//
// It builds the archive at a temporary path and renames it into place, so an
// interrupted backup never leaves a truncated file that looks restorable.
func Create(storageDir, destPath string) error {
	info, err := os.Stat(storageDir)
	if err != nil {
		return fmt.Errorf("backup source %s: %w", storageDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup source %s is not a directory", storageDir)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".outpost-backup-*")
	if err != nil {
		return fmt.Errorf("create temp archive: %w", err)
	}
	tmpPath := tmp.Name()
	// Any early return past this point must not leave the temp file behind.
	defer func() {
		tmp.Close()
		os.Remove(tmpPath)
	}()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)

	err = filepath.Walk(storageDir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(storageDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}

		// Symlinks are stored as links rather than followed, so a link out of
		// the storage tree cannot silently pull in unrelated files.
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return fmt.Errorf("archive %s: %w", storageDir, err)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("finalise tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finalise gzip: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("flush archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close archive: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("finalise archive: %w", err)
	}
	return os.Chmod(destPath, 0o600)
}

// Extract replaces storageDir's contents with the archive's.
//
// The old directory is moved aside and only removed once extraction succeeds,
// so a failed restore does not destroy the data it was replacing.
func Extract(archivePath, storageDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read archive %s: %w", archivePath, err)
	}
	defer gz.Close()

	parent := filepath.Dir(storageDir)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", parent, err)
	}
	staging, err := os.MkdirTemp(parent, ".outpost-restore-*")
	if err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	defer os.RemoveAll(staging)

	if err := untar(tar.NewReader(gz), staging); err != nil {
		return err
	}

	// Swap staging into place, keeping the previous contents until the new
	// directory is in position.
	previous := ""
	if _, err := os.Stat(storageDir); err == nil {
		previous, err = os.MkdirTemp(parent, ".outpost-previous-*")
		if err != nil {
			return fmt.Errorf("stage previous contents: %w", err)
		}
		os.RemoveAll(previous)
		if err := os.Rename(storageDir, previous); err != nil {
			return fmt.Errorf("move aside %s: %w", storageDir, err)
		}
	}

	if err := os.Rename(staging, storageDir); err != nil {
		// Put the original back rather than leaving nothing behind. If even
		// that fails the storage directory no longer exists, and saying so is
		// the difference between a failed restore and silent data loss.
		if previous != "" {
			if rollback := os.Rename(previous, storageDir); rollback != nil {
				return fmt.Errorf("install restored data: %w (and the original could not be put back, it is at %s: %v)", err, previous, rollback)
			}
		}
		return fmt.Errorf("install restored data: %w", err)
	}
	if previous != "" {
		os.RemoveAll(previous)
	}
	return os.Chmod(storageDir, 0o700)
}

// untar writes entries under target, refusing anything that escapes it.
//
// Containment needs two separate checks. safeJoin rejects entry *names* that
// escape lexically, but that is not sufficient on its own: an archive can
// create a symlink and then write a later entry through it, so every entry's
// parent components are also checked for symlinks before anything is written.
func untar(tr *tar.Reader, target string) error {
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read archive entry: %w", err)
		}

		path, err := safeJoin(target, header.Name)
		if err != nil {
			return err
		}
		// Nothing may be written through a symlink the archive itself planted.
		if err := checkNoSymlinkParent(target, header.Name); err != nil {
			return err
		}

		// An entry the archive already planted as a symlink must not be
		// written through either: MkdirAll and O_CREATE both follow it.
		if header.Typeflag == tar.TypeDir || header.Typeflag == tar.TypeReg {
			if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("archive entry %q is written through a symlink the archive created", header.Name)
			}
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, os.FileMode(header.Mode).Perm()); err != nil {
				return fmt.Errorf("create %s: %w", path, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
			}
			out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY|syscall.O_NOFOLLOW, os.FileMode(header.Mode).Perm())
			if err != nil {
				return fmt.Errorf("create %s: %w", path, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", path, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", path, err)
			}
		case tar.TypeSymlink:
			if err := checkLinkTarget(header.Name, header.Linkname); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.Symlink(header.Linkname, path); err != nil {
				return fmt.Errorf("symlink %s: %w", path, err)
			}
		default:
			// Devices, fifos and hard links are not what a service's storage
			// directory should contain; skipping them keeps restore predictable.
			continue
		}
	}
}

// checkLinkTarget rejects a symlink whose target leaves the archive tree.
//
// An absolute target must be refused outright: filepath.Join drops the leading
// separator, so an absolute Linkname would otherwise look contained while
// os.Symlink still creates a genuinely absolute link.
//
// The lexical check alone is not enough either: in "sub/link/.." the ".."
// is applied on disk after following sub/link, which an earlier (or later)
// entry may have made a symlink, so Clean's answer is not where it lands. A
// ".." is therefore only accepted before any named component, where it walks
// up real directories from the link's own; the descent after that only
// passes through links that were held to the same rule.
func checkLinkTarget(name, linkname string) error {
	if linkname == "" {
		return fmt.Errorf("archive entry %q is a symlink with no target", name)
	}
	if filepath.IsAbs(linkname) || strings.HasPrefix(linkname, "/") {
		return fmt.Errorf("archive entry %q points at absolute path %q, which escapes the target directory", name, linkname)
	}
	named := false
	for _, part := range strings.Split(filepath.ToSlash(linkname), "/") {
		switch {
		case part == "..":
			if named {
				return fmt.Errorf("archive entry %q points at %q, which climbs out of a path that may be a symlink", name, linkname)
			}
		case part != "" && part != ".":
			named = true
		}
	}
	// Resolve the link relative to its own directory, as the filesystem would.
	resolved := filepath.Join(filepath.Dir(filepath.FromSlash(name)), filepath.FromSlash(linkname))
	if resolved == ".." || strings.HasPrefix(resolved, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("archive entry %q points at %q, which escapes the target directory", name, linkname)
	}
	return nil
}

// checkNoSymlinkParent fails if any already-extracted parent component of name
// is a symlink, which would redirect the write outside the tree.
func checkNoSymlinkParent(root, name string) error {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(filepath.FromSlash(name))), "/")
	current := root
	// Parents only: the entry itself is allowed to be a symlink.
	for _, part := range parts[:max(len(parts)-1, 0)] {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		fi, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // nothing extracted here yet
			}
			return fmt.Errorf("inspect %s: %w", current, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("archive entry %q is written through symlink %q, which escapes the target directory", name, part)
		}
	}
	return nil
}

// safeJoin resolves name under root, rejecting absolute paths and traversal.
func safeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is an absolute path and escapes the target directory", name)
	}
	joined := filepath.Join(root, filepath.FromSlash(name))
	cleanRoot := filepath.Clean(root)
	if joined != cleanRoot && !strings.HasPrefix(joined, cleanRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the target directory", name)
	}
	return joined, nil
}

// List returns a service's archives, newest first.
func List(root, service string) ([]Archive, error) {
	dir := filepath.Join(root, service)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backups for %s: %w", service, err)
	}

	var out []Archive
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), archiveExt) {
			continue
		}
		stamp := strings.TrimSuffix(e.Name(), archiveExt)
		at, err := time.Parse(TimestampFormat, stamp)
		if err != nil {
			continue // not one of ours
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Archive{
			Service:   service,
			Timestamp: stamp,
			Path:      filepath.Join(dir, e.Name()),
			Size:      info.Size(),
			CreatedAt: at,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Find locates one archive by timestamp. An empty timestamp means the newest.
func Find(root, service, timestamp string) (Archive, error) {
	archives, err := List(root, service)
	if err != nil {
		return Archive{}, err
	}
	if len(archives) == 0 {
		return Archive{}, fmt.Errorf("no backups for %s", service)
	}
	if timestamp == "" {
		return archives[0], nil
	}
	for _, a := range archives {
		if a.Timestamp == timestamp {
			return a, nil
		}
	}
	return Archive{}, fmt.Errorf("no backup for %s at %s", service, timestamp)
}

package acquire

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Archive kinds Outpost can take a binary out of. Most GitHub releases ship
// one of these rather than a bare executable, so refusing them made `outpost
// add <url>` fail for the majority of real repositories.
const (
	archiveNone = iota
	archiveTarGz
	archiveZip
)

// archiveExts is the one table of archive suffixes Outpost opens. archiveKind
// and stripArchiveExt both read it, so adding a format is a single edit.
var archiveExts = []struct {
	ext  string
	kind int
}{
	{".tar.gz", archiveTarGz},
	{".tgz", archiveTarGz},
	{".zip", archiveZip},
}

// archiveKind classifies an asset name.
func archiveKind(name string) int {
	lower := strings.ToLower(name)
	for _, a := range archiveExts {
		if strings.HasSuffix(lower, a.ext) {
			return a.kind
		}
	}
	return archiveNone
}

// stripArchiveExt drops a recognised archive suffix so two names can be
// compared on what they actually say.
func stripArchiveExt(name string) string {
	lower := strings.ToLower(name)
	for _, a := range archiveExts {
		if strings.HasSuffix(lower, a.ext) {
			return name[:len(name)-len(a.ext)]
		}
	}
	return name
}

// unsupportedArchiveExts are archives Outpost will not open: they need tooling
// it does not carry, or they are package formats with their own installer.
// A bare ".gz" or ".xz" is a compressed binary rather than a binary, and
// installing the compressed bytes would fail at exec time.
var unsupportedArchiveExts = []string{
	".tar.xz", ".tar.bz2", ".tar.zst", ".tar", ".gz", ".xz", ".bz2", ".zst",
	".7z", ".deb", ".rpm", ".apk", ".pkg", ".dmg", ".msi",
}

// signatureExts are never installable under any circumstances: signatures,
// attestations and the text that travels with them.
var signatureExts = []string{
	".sig", ".asc", ".pem", ".pub", ".crt", ".cert", ".minisig", ".sbom", ".spdx",
	".bundle", ".sigstore", ".jsonl", ".json", ".txt", ".md",
}

func hasSuffixAny(lower string, exts []string) bool {
	for _, ext := range exts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// isSignature reports an asset that accompanies a release rather than being
// part of it.
func isSignature(name string) bool { return hasSuffixAny(strings.ToLower(name), signatureExts) }

// isUnsupportedArchive reports an archive Outpost cannot open. A supported
// archive is checked first because ".gz" is also the tail of ".tar.gz".
func isUnsupportedArchive(name string) bool {
	if archiveKind(name) != archiveNone {
		return false
	}
	return hasSuffixAny(strings.ToLower(name), unsupportedArchiveExts)
}

// nonBinaryDirs are directories a release archive uses for things that are
// never the binary, even when a file in one carries the binary's own name --
// bash completions are conventionally a file named exactly after the command.
var nonBinaryDirs = map[string]bool{
	"completions": true, "completion": true, "autocomplete": true,
	"man": true, "doc": true, "docs": true,
}

// docPrefixes name the paperwork that sits beside a binary. A prefix only
// counts when it is the whole name or is followed by punctuation, so LICENSE,
// LICENSE-MIT and README.md are caught.
var docPrefixes = []string{"license", "licence", "readme", "changelog", "notice", "copying", "authors", "contributing"}

// docExts are suffixes of files that are never the binary. ".1" and ".5" are
// man pages, but a version-numbered executable can end the same way, so those
// two are only trusted on a file without its executable bit.
var docExts = []string{".html", ".yml", ".yaml", ".toml", ".service", ".sh", ".bash", ".zsh", ".fish", ".ps1"}

// inNonBinaryDir reports a member under completions/, man/ and the like.
func inNonBinaryDir(name string) bool {
	dirs := strings.Split(strings.ToLower(path.Dir(name)), "/")
	for _, d := range dirs {
		if nonBinaryDirs[d] {
			return true
		}
	}
	return false
}

// skipInArchive reports files that sit beside a binary in a release archive
// and are never the binary. It is only consulted when no member carries the
// service's name: an exact name match is taken on its own evidence, so a
// service called readme-server is not mistaken for its own readme.
func skipInArchive(m member) bool {
	if inNonBinaryDir(m.name) || isSignature(m.name) {
		return true
	}
	base := strings.ToLower(path.Base(m.name))
	if strings.HasPrefix(base, ".") {
		return true
	}
	for _, prefix := range docPrefixes {
		if base == prefix {
			return true
		}
		if rest, ok := strings.CutPrefix(base, prefix); ok && strings.ContainsRune(".-_", rune(rest[0])) {
			return true
		}
	}
	if hasSuffixAny(base, docExts) {
		return true
	}
	if !m.executable && (strings.HasSuffix(base, ".1") || strings.HasSuffix(base, ".5")) {
		return true
	}
	return false
}

// windowsOnlyExts are executables that cannot run anywhere but Windows.
var windowsOnlyExts = []string{".exe", ".dll", ".bat", ".cmd"}

// member is one candidate file inside an archive.
type member struct {
	name       string // full path within the archive
	executable bool
	size       int64
	index      int // position in the archive, which is unique where names need not be
}

// maxExpandedBytes bounds how much an archive may decompress to in total.
// Only the chosen member was bounded before, so a gzip bomb with many entries
// could be ground through without limit while choosing one.
const maxExpandedBytes = 8 * maxAssetBytes

var errExpandedTooLarge = fmt.Errorf("decompresses to more than the %d MiB limit", maxExpandedBytes>>20)

// boundedReader fails with errExpandedTooLarge rather than a bare EOF, which
// the tar reader would report as a truncated archive.
type boundedReader struct {
	r    io.Reader
	left int64
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errExpandedTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	return n, err
}

// extractBinary copies the service's executable out of the archive at src into
// dest, and reports which member it took.
//
// It never writes the archive's own paths. Only one file is produced, at a
// location the caller chose, so a "zip slip" entry named "../../etc/cron.d/x"
// cannot escape anywhere — the name is used to *choose* a member, never to
// place one. Symlinks and directories are ignored for the same reason: a
// binary is a regular file, and anything else in there is not what we came
// for.
func extractBinary(src, dest, wantName string, kind int) (string, error) {
	return extractBinaryFor(src, dest, wantName, kind, runtime.GOOS)
}

// extractBinaryFor is extractBinary for a named target platform, which decides
// whether a Windows executable is a candidate at all.
func extractBinaryFor(src, dest, wantName string, kind int, goos string) (string, error) {
	switch kind {
	case archiveTarGz:
		return extractTarGz(src, dest, wantName, goos)
	case archiveZip:
		return extractZip(src, dest, wantName, goos)
	}
	return "", fmt.Errorf("unsupported archive")
}

// extractTarGz reads the stream once. A tar has no index, so finding the
// member and copying it in separate passes decompressed the whole archive
// twice; instead every member that could be chosen is spooled beside dest as
// it goes by, and the chosen one is renamed into place.
func extractTarGz(src, dest, wantName, goos string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", fmt.Errorf("read gzip: %w", err)
	}
	defer gz.Close()

	spool, err := os.MkdirTemp(filepath.Dir(dest), ".outpost-members-*")
	if err != nil {
		return "", err
	}
	// Best-effort cleanup of the losing candidates; the chosen one has already
	// been renamed out, and a leftover dot-directory is harmless.
	defer func() { _ = os.RemoveAll(spool) }()

	var members []member
	tr := tar.NewReader(&boundedReader{r: gz, left: maxExpandedBytes})
	for i := 0; ; i++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		m := member{
			name:       h.Name,
			executable: h.FileInfo().Mode().Perm()&0o111 != 0,
			size:       h.Size,
			index:      i,
		}
		members = append(members, m)
		if !mayChoose(m, wantName, goos) {
			continue
		}
		// Spooled under its index, never its archive path, so the name still
		// only selects and never places.
		if err := copyBounded(filepath.Join(spool, strconv.Itoa(i)), tr, m.name); err != nil {
			return "", err
		}
	}

	chosen, err := chooseMember(members, wantName, goos)
	if err != nil {
		return "", err
	}
	if err := os.Rename(filepath.Join(spool, strconv.Itoa(chosen.index)), dest); err != nil {
		return "", fmt.Errorf("extract %s: %w", chosen.name, err)
	}
	return chosen.name, nil
}

// extractZip lists the central directory, which costs no decompression, and
// opens the chosen entry by position. Zip permits duplicate names, so opening
// by name could copy a different entry -- a symlink, say -- than the regular
// file that was chosen.
func extractZip(src, dest, wantName, goos string) (string, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return "", fmt.Errorf("read zip: %w", err)
	}
	defer zr.Close()

	var members []member
	for i, f := range zr.File {
		if !f.Mode().IsRegular() {
			continue
		}
		members = append(members, member{
			name:       f.Name,
			executable: f.Mode().Perm()&0o111 != 0,
			size:       int64(f.UncompressedSize64),
			index:      i,
		})
	}
	chosen, err := chooseMember(members, wantName, goos)
	if err != nil {
		return "", err
	}

	rc, err := zr.File[chosen.index].Open()
	if err != nil {
		return "", fmt.Errorf("extract %s: %w", chosen.name, err)
	}
	defer rc.Close()
	if err := copyBounded(dest, rc, chosen.name); err != nil {
		return "", err
	}
	return chosen.name, nil
}

// copyBounded writes one member to dest, bounded like any other download: an
// archive can claim a far larger uncompressed size than it occupies.
func copyBounded(dest string, r io.Reader, name string) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, maxAssetBytes+1))
	if err != nil {
		// The copy error is the one worth reporting; the file is discarded.
		_ = out.Close()
		if errors.Is(err, errExpandedTooLarge) {
			return fmt.Errorf("the archive %w", err)
		}
		return fmt.Errorf("extract %s: %w", name, err)
	}
	if n > maxAssetBytes {
		// As above: the size is the failure, and the file is discarded.
		_ = out.Close()
		return fmt.Errorf("extract %s: larger than the %d MiB limit", name, maxAssetBytes>>20)
	}
	return out.Close()
}

// matchesName reports a member named after the service. On Windows the
// ".exe" spelling is the same binary; anywhere else it is a PE file that
// cannot exec, so it is no match at all.
func matchesName(m member, wantName, goos string) bool {
	base := path.Base(m.name)
	if strings.EqualFold(base, wantName) {
		return true
	}
	return goos == "windows" && strings.EqualFold(strings.TrimSuffix(strings.ToLower(base), ".exe"), wantName)
}

// candidate reports a member that the fallback -- no name match -- may pick.
func candidate(m member, goos string) bool {
	if skipInArchive(m) {
		return false
	}
	return goos == "windows" || !hasSuffixAny(strings.ToLower(m.name), windowsOnlyExts)
}

// mayChoose reports a member chooseMember could return, so the tar path only
// spools those.
func mayChoose(m member, wantName, goos string) bool {
	return (matchesName(m, wantName, goos) && !inNonBinaryDir(m.name)) || candidate(m, goos)
}

// chooseMember picks the binary. A file named after the service wins outright;
// otherwise the sole executable does. Anything else is ambiguous and is an
// error naming what was found, because guessing wrong installs the wrong file
// and the failure surfaces much later as an exec error.
func chooseMember(members []member, wantName, goos string) (member, error) {
	var named []member
	for _, m := range members {
		if matchesName(m, wantName, goos) && !inNonBinaryDir(m.name) {
			named = append(named, m)
		}
	}
	if len(named) == 1 {
		return named[0], nil
	}
	if len(named) > 1 {
		// Several files carry the name. The executable one is the binary;
		// failing that, picking one is a guess.
		if exec := executables(named); len(exec) == 1 {
			return exec[0], nil
		}
		return member{}, ambiguous(named, wantName)
	}

	var usable []member
	for _, m := range members {
		if candidate(m, goos) {
			usable = append(usable, m)
		}
	}
	if len(usable) == 0 {
		return member{}, fmt.Errorf("the archive holds no file that looks like a binary")
	}
	if exec := executables(usable); len(exec) == 1 {
		return exec[0], nil
	}
	if len(usable) == 1 {
		return usable[0], nil
	}
	return member{}, ambiguous(usable, wantName)
}

func executables(ms []member) []member {
	var out []member
	for _, m := range ms {
		if m.executable {
			out = append(out, m)
		}
	}
	return out
}

func ambiguous(ms []member, wantName string) error {
	names := make([]string, 0, len(ms))
	for _, m := range ms {
		names = append(names, m.name)
	}
	sort.Strings(names)
	return fmt.Errorf("the archive holds %d candidate files (%s) and no single one is the binary %q; "+
		"name the service with --name, or the repository can set `name` in %s",
		len(names), strings.Join(names, ", "), wantName, DescriptorName)
}

package acquire

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
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

// archiveKind classifies an asset name.
func archiveKind(name string) int {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return archiveTarGz
	case strings.HasSuffix(lower, ".zip"):
		return archiveZip
	}
	return archiveNone
}

// unsupportedArchiveExts are archives Outpost will not open: they need tooling
// it does not carry, or they are package formats with their own installer.
var unsupportedArchiveExts = []string{".tar.xz", ".tar.bz2", ".tar.zst", ".7z", ".deb", ".rpm", ".pkg", ".dmg", ".msi"}

// signatureExts are never installable under any circumstances.
var signatureExts = []string{".sig", ".asc", ".pem", ".pub", ".minisig", ".sbom", ".json", ".txt", ".md"}

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

// isUnsupportedArchive reports an archive Outpost cannot open.
func isUnsupportedArchive(name string) bool {
	return hasSuffixAny(strings.ToLower(name), unsupportedArchiveExts)
}

// skipInArchive are files that sit beside a binary in a release archive and
// are never the binary.
func skipInArchive(name string) bool {
	base := strings.ToLower(path.Base(name))
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "._") {
		return true
	}
	for _, prefix := range []string{"license", "licence", "readme", "changelog", "notice", "copying", "authors", "contributing"} {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	for _, ext := range []string{".md", ".txt", ".html", ".1", ".5", ".yml", ".yaml", ".json", ".toml", ".service", ".sh", ".bash", ".zsh", ".fish", ".ps1"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

// member is one candidate file inside an archive.
type member struct {
	name       string // full path within the archive
	executable bool
	size       int64
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
	members, err := listMembers(src, kind)
	if err != nil {
		return "", err
	}
	chosen, err := chooseMember(members, wantName)
	if err != nil {
		return "", err
	}
	if err := copyMember(src, dest, chosen, kind); err != nil {
		return "", err
	}
	return chosen, nil
}

// listMembers enumerates the regular files in an archive.
func listMembers(src string, kind int) ([]member, error) {
	switch kind {
	case archiveTarGz:
		return listTarGz(src)
	case archiveZip:
		return listZip(src)
	}
	return nil, fmt.Errorf("unsupported archive")
}

func listTarGz(src string) ([]member, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("read gzip: %w", err)
	}
	defer gz.Close()

	var out []member
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read tar: %w", err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		out = append(out, member{
			name:       h.Name,
			executable: h.FileInfo().Mode().Perm()&0o111 != 0,
			size:       h.Size,
		})
	}
	return out, nil
}

func listZip(src string) ([]member, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, fmt.Errorf("read zip: %w", err)
	}
	defer zr.Close()

	var out []member
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || !f.Mode().IsRegular() {
			continue
		}
		out = append(out, member{
			name:       f.Name,
			executable: f.Mode().Perm()&0o111 != 0,
			size:       int64(f.UncompressedSize64),
		})
	}
	return out, nil
}

// chooseMember picks the binary. A file named after the service wins outright;
// otherwise the sole executable does. Anything else is ambiguous and is an
// error naming what was found, because guessing wrong installs the wrong file
// and the failure surfaces much later as an exec error.
func chooseMember(members []member, wantName string) (string, error) {
	var usable []member
	for _, m := range members {
		if skipInArchive(m.name) || isSignature(m.name) {
			continue
		}
		usable = append(usable, m)
	}
	if len(usable) == 0 {
		return "", fmt.Errorf("the archive holds no file that looks like a binary")
	}

	for _, m := range usable {
		if strings.EqualFold(path.Base(m.name), wantName) {
			return m.name, nil
		}
	}
	// A Windows-style name is still the right binary on the wrong platform;
	// matching it keeps the error honest rather than reporting ambiguity.
	for _, m := range usable {
		if strings.EqualFold(strings.TrimSuffix(path.Base(m.name), ".exe"), wantName) {
			return m.name, nil
		}
	}

	var executables []member
	for _, m := range usable {
		if m.executable {
			executables = append(executables, m)
		}
	}
	if len(executables) == 1 {
		return executables[0].name, nil
	}
	if len(usable) == 1 {
		return usable[0].name, nil
	}

	names := make([]string, 0, len(usable))
	for _, m := range usable {
		names = append(names, m.name)
	}
	sort.Strings(names)
	return "", fmt.Errorf("the archive holds %d candidate files (%s) and none is named %q; "+
		"name the service with --name, or the repository can set `name` in %s",
		len(names), strings.Join(names, ", "), wantName, DescriptorName)
}

// copyMember writes one member of the archive to dest.
func copyMember(src, dest, want string, kind int) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	var rc io.ReadCloser
	switch kind {
	case archiveTarGz:
		rc, err = openTarMember(src, want)
	case archiveZip:
		rc, err = openZipMember(src, want)
	default:
		return fmt.Errorf("unsupported archive")
	}
	if err != nil {
		return err
	}
	defer rc.Close()

	// Bounded like any other download: an archive can claim a far larger
	// uncompressed size than it occupies.
	n, err := io.Copy(out, io.LimitReader(rc, maxAssetBytes+1))
	if err != nil {
		return fmt.Errorf("extract %s: %w", want, err)
	}
	if n > maxAssetBytes {
		return fmt.Errorf("extract %s: larger than the %d MiB limit", want, maxAssetBytes>>20)
	}
	return out.Close()
}

// tarMember pairs the entry reader with the files that must outlive it.
type tarMember struct {
	io.Reader
	closers []io.Closer
}

func (t *tarMember) Close() error {
	for _, c := range t.closers {
		_ = c.Close()
	}
	return nil
}

func openTarMember(src, want string) (io.ReadCloser, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			gz.Close()
			f.Close()
			return nil, fmt.Errorf("%s not found in the archive", want)
		}
		if h.Typeflag == tar.TypeReg && h.Name == want {
			return &tarMember{Reader: tr, closers: []io.Closer{gz, f}}, nil
		}
	}
}

func openZipMember(src, want string) (io.ReadCloser, error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Name == want {
			rc, err := f.Open()
			if err != nil {
				zr.Close()
				return nil, err
			}
			return &tarMember{Reader: rc, closers: []io.Closer{rc, zr}}, nil
		}
	}
	zr.Close()
	return nil, fmt.Errorf("%s not found in the archive", want)
}

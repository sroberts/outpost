package acquire

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Archive kinds Outpost can take a binary out of. Most GitHub releases ship
// one of these rather than a bare executable, so refusing them made `outpost
// add <url>` fail for the majority of real repositories.
const (
	archiveNone = iota
	archiveTarGz
	archiveZip
)

// archiveExts is the one table of suffixes Outpost unpacks. Classifying and
// stripping both read it, so adding a format cannot update one and not the
// other.
var archiveExts = []struct {
	ext  string
	kind int
}{
	{".tar.gz", archiveTarGz},
	{".tgz", archiveTarGz},
	{".zip", archiveZip},
}

// splitArchiveExt separates a supported archive suffix from the name before it.
func splitArchiveExt(name string) (string, int) {
	lower := strings.ToLower(name)
	for _, a := range archiveExts {
		if strings.HasSuffix(lower, a.ext) {
			return name[:len(name)-len(a.ext)], a.kind
		}
	}
	return name, archiveNone
}

// archiveKind classifies an asset name.
func archiveKind(name string) int {
	_, kind := splitArchiveExt(name)
	return kind
}

// stripArchiveExt drops a recognised archive suffix so two names can be
// compared on what they actually say.
func stripArchiveExt(name string) string {
	stem, _ := splitArchiveExt(name)
	return stem
}

// installable reports whether an asset has the shape of something Outpost can
// install: an archive it unpacks, or a bare executable.
//
// It is an allowlist on purpose. The denylist it replaced (.sig, .tar.xz, .deb,
// …) was never complete, and every gap — a .tar.gz.sha512, a .gz, an .apk —
// was chmod +x'd and installed as the service, failing only at exec time.
func installable(name, goos string) bool {
	return archiveKind(name) != archiveNone || isBareExecutable(name, goos)
}

// isBareExecutable reports a name with no file extension, which is how
// executables are named everywhere but Windows, or a ".bin" that some
// projects add to say exactly that. A final dot segment holding a
// separator or only digits is a version ("svc_1.2.3_linux_amd64",
// "svc-1.2.5"), not an extension.
func isBareExecutable(name, goos string) bool {
	base := path.Base(name)
	i := strings.LastIndexByte(base, '.')
	if i < 0 {
		return true
	}
	ext := strings.ToLower(base[i+1:])
	switch {
	case ext == "exe":
		return goos == "windows"
	case ext == "bin":
		return true
	case ext == "":
		return false
	case strings.ContainsAny(ext, "-_"):
		return true
	}
	for _, r := range ext {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// Printable returns s unchanged, or Go-quoted when it holds anything that is
// not printable. Archive member names are arbitrary bytes chosen by whoever
// built the archive — unlike asset names, which GitHub sanitises — and an
// escape sequence printed verbatim can rewrite the operator's terminal,
// including the line saying whether the download was verified.
func Printable(s string) string {
	if !utf8.ValidString(s) {
		return strconv.Quote(s)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return strconv.Quote(s)
		}
	}
	return s
}

// Listing an archive decompresses all of it, and a small tar.gz of empty
// headers can expand by orders of magnitude. These bound that work; the
// chosen member is separately bounded by maxAssetBytes.
const (
	maxArchiveEntries = 100_000
	maxUnpackedBytes  = 4 * maxAssetBytes
)

// unpackLimitError reports a stream that ran past its byte limit.
type unpackLimitError int64

func (e unpackLimitError) Error() string {
	return fmt.Sprintf("larger than the %d MiB limit", int64(e)>>20)
}

// boundedReader stops at a byte limit and when ctx is done, so unpacking is
// covered by the same deadline as the download that preceded it.
type boundedReader struct {
	ctx   context.Context
	r     io.Reader
	limit int64
	left  int64 // bytes still allowed, plus one
}

func newBoundedReader(ctx context.Context, r io.Reader, limit int64) *boundedReader {
	return &boundedReader{ctx: ctx, r: r, limit: limit, left: limit + 1}
}

func (b *boundedReader) Read(p []byte) (int, error) {
	if err := b.ctx.Err(); err != nil {
		return 0, err
	}
	if b.left <= 0 {
		return 0, unpackLimitError(b.limit)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if b.left <= 0 {
		return n, unpackLimitError(b.limit)
	}
	return n, err
}

// docNames ship beside a binary and are never it. They match as a whole word
// ("LICENSE", "LICENSE-MIT"), not as a prefix, so a binary called "licensed"
// or "noticeboard" is not thrown away.
var docNames = []string{"license", "licence", "readme", "changelog", "notice", "copying", "authors", "contributing"}

func isDocName(base string) bool {
	lower := strings.ToLower(base)
	for _, d := range docNames {
		if lower == d || (strings.HasPrefix(lower, d) && strings.ContainsRune("-_.", rune(lower[len(d)]))) {
			return true
		}
	}
	return false
}

// member is one regular file inside an archive.
type member struct {
	name  string // full path within the archive
	index int    // entry position, so the copy opens exactly the entry judged
	// executable is false only when the archive records a mode and it has no
	// execute bit. Zips written on Windows record none, and treating those as
	// non-executable would make their binaries unfindable.
	executable bool
}

// extractBinary is extractMember for the host platform, with no deadline and a
// single name to look for.
func extractBinary(src, dest, wantName string, kind int) (string, error) {
	return extractMember(context.Background(), src, dest, kind, runtime.GOOS, []string{wantName})
}

// extractMember copies the service's executable out of the archive at src
// into dest, and reports which member it took. wantNames are the names the
// binary may go by, most preferred first.
//
// It never writes the archive's own paths. Only one file is produced, at a
// location the caller chose, so a "zip slip" entry named "../../etc/cron.d/x"
// cannot escape anywhere — the name is used to *choose* a member, never to
// place one. Symlinks and directories are ignored for the same reason: a
// binary is a regular file, and anything else in there is not what we came
// for.
func extractMember(ctx context.Context, src, dest string, kind int, goos string, wantNames []string) (string, error) {
	switch kind {
	case archiveTarGz:
		members, err := listTarGz(ctx, src)
		if err != nil {
			return "", err
		}
		chosen, err := chooseMember(members, wantNames, goos)
		if err != nil {
			return "", err
		}
		// A second pass over the stream: gzip cannot seek, and spooling every
		// candidate to disk during the first pass to avoid it would cost more
		// than decompressing twice.
		err = walkTarGz(ctx, src, func(i int, _ *tar.Header, r io.Reader) (bool, error) {
			if i != chosen.index {
				return false, nil
			}
			return true, writeMember(ctx, r, dest, chosen.name)
		})
		return chosen.name, err

	case archiveZip:
		zr, err := zip.OpenReader(src)
		if err != nil {
			return "", fmt.Errorf("read zip: %w", err)
		}
		defer zr.Close()
		if len(zr.File) > maxArchiveEntries {
			return "", fmt.Errorf("the archive holds more than %d entries", maxArchiveEntries)
		}
		chosen, err := chooseMember(zipMembers(zr), wantNames, goos)
		if err != nil {
			return "", err
		}
		rc, err := zr.File[chosen.index].Open()
		if err != nil {
			return "", fmt.Errorf("extract %s: %w", Printable(chosen.name), err)
		}
		defer rc.Close()
		return chosen.name, writeMember(ctx, rc, dest, chosen.name)
	}
	return "", fmt.Errorf("unsupported archive")
}

// walkTarGz calls fn for each entry until fn reports it is done, bounded in
// entries, decompressed bytes and time.
func walkTarGz(ctx context.Context, src string, fn func(i int, h *tar.Header, r io.Reader) (bool, error)) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("read gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(newBoundedReader(ctx, gz, maxUnpackedBytes))
	for i := 0; ; i++ {
		if i >= maxArchiveEntries {
			return fmt.Errorf("the archive holds more than %d entries", maxArchiveEntries)
		}
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		done, err := fn(i, h, tr)
		if err != nil || done {
			return err
		}
	}
}

// listTarGz enumerates the regular files in a tar.gz. A name that appears
// twice keeps only its last entry, which is the one tar itself would extract.
func listTarGz(ctx context.Context, src string) ([]member, error) {
	var out []member
	seen := map[string]int{}
	err := walkTarGz(ctx, src, func(i int, h *tar.Header, _ io.Reader) (bool, error) {
		if h.Typeflag != tar.TypeReg {
			return false, nil
		}
		m := member{name: h.Name, index: i, executable: h.FileInfo().Mode().Perm()&0o111 != 0}
		if at, ok := seen[h.Name]; ok {
			out[at] = m
			return false, nil
		}
		seen[h.Name] = len(out)
		out = append(out, m)
		return false, nil
	})
	return out, err
}

// zipMembers enumerates the regular files in a zip, with the same
// last-entry-wins rule for repeated names as listTarGz.
func zipMembers(zr *zip.ReadCloser) []member {
	var out []member
	seen := map[string]int{}
	for i, f := range zr.File {
		if !f.Mode().IsRegular() {
			continue
		}
		// Only Unix and macOS creators record a mode worth reading.
		creator := f.CreatorVersion >> 8
		modeKnown := creator == 3 || creator == 19
		m := member{name: f.Name, index: i, executable: !modeKnown || f.Mode().Perm()&0o111 != 0}
		if at, ok := seen[f.Name]; ok {
			out[at] = m
			continue
		}
		seen[f.Name] = len(out)
		out = append(out, m)
	}
	return out
}

// chooseMember picks the binary. An executable named for the service — or for
// the program the asset is named after — wins; otherwise the sole executable
// does. Anything else is an error naming what was found, because guessing
// wrong installs the wrong file and the failure surfaces much later as an
// exec error.
func chooseMember(members []member, wantNames []string, goos string) (member, error) {
	var shaped []member
	sawExe := false
	for _, m := range members {
		base := path.Base(m.name)
		if strings.HasPrefix(base, ".") {
			continue
		}
		// The same shape test as for release assets: no extension means no
		// nested archive, script, man page or data file installed as the
		// binary.
		if !isBareExecutable(base, goos) {
			sawExe = sawExe || strings.EqualFold(path.Ext(base), ".exe")
			continue
		}
		shaped = append(shaped, m)
	}

	named := func(from []member, want string) []member {
		var out []member
		for _, m := range from {
			base := path.Base(m.name)
			if strings.EqualFold(base, want) || (goos == "windows" && strings.EqualFold(base, want+".exe")) {
				out = append(out, m)
			}
		}
		return out
	}

	// A name match beats everything, but only an executable one: a
	// completion script or doc page sharing the service's name is not it.
	for _, want := range wantNames {
		switch execs := executables(named(shaped, want)); len(execs) {
		case 0:
			continue
		case 1:
			return execs[0], nil
		default:
			return member{}, fmt.Errorf("the archive holds %d executables named %q (%s); "+
				"which one is the service cannot be told from the archive, so nothing was installed",
				len(execs), want, quotedNames(execs))
		}
	}

	var usable []member
	for _, m := range shaped {
		if !isDocName(path.Base(m.name)) {
			usable = append(usable, m)
		}
	}
	execs := executables(usable)
	if len(execs) == 1 {
		return execs[0], nil
	}
	if len(execs) == 0 {
		// Some archivers drop execute bits altogether. A file named for the
		// service is then the best evidence left, but only when it is the
		// only one.
		for _, want := range wantNames {
			if n := named(shaped, want); len(n) == 1 {
				return n[0], nil
			}
		}
		if sawExe && len(usable) == 0 {
			return member{}, fmt.Errorf("the archive holds only Windows executables (.exe), which cannot run on %s", goos)
		}
		if len(usable) == 0 {
			return member{}, fmt.Errorf("the archive holds no file that looks like a binary")
		}
		execs = usable
	}

	return member{}, fmt.Errorf("the archive holds %d candidate files (%s) and none is named %q; "+
		"name the service with --name, or the repository can set `name` in %s",
		len(execs), quotedNames(execs), wantNames[0], DescriptorName)
}

func executables(members []member) []member {
	var out []member
	for _, m := range members {
		if m.executable {
			out = append(out, m)
		}
	}
	return out
}

// quotedNames lists member names for an error, quoted because they come from
// the archive and may hold anything.
func quotedNames(members []member) string {
	names := make([]string, 0, len(members))
	for _, m := range members {
		names = append(names, strconv.Quote(m.name))
	}
	return strings.Join(names, ", ")
}

// writeMember copies one member's contents to dest.
func writeMember(ctx context.Context, r io.Reader, dest, name string) error {
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer out.Close()

	// Bounded like any other download: an archive can claim a far larger
	// uncompressed size than it occupies.
	if _, err := io.Copy(out, newBoundedReader(ctx, r, maxAssetBytes)); err != nil {
		return fmt.Errorf("extract %s: %w", Printable(name), err)
	}
	return out.Close()
}

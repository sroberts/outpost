package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Result describes what was installed.
type Result struct {
	Tag   string
	Asset string
	Path  string
	// Member is the file taken out of an archive, empty when the asset was
	// already a bare binary.
	Member string
	// Verified reports that the asset's SHA-256 matched a checksum the release
	// published. False means the release published none — see Warning.
	Verified bool
	Warning  string
}

// checksumNames are the conventional names for a release's checksum manifest,
// in the order they are preferred. All of these hold `sha256sum` output.
//
// A bare "checksums" is deliberately absent. It looks like it belongs, but
// projects using that name often do not use that format -- mikefarah/yq, for
// one, publishes "<filename>  <crc32>  <md5>  <md5>  <sha1>  <sha256>" with
// the column order in a separate file. Reading it as sha256sum output would
// take the filename as the hash, mismatch, and refuse a perfectly good binary.
// Warning that a release publishes nothing we can read is the better failure.
var checksumNames = []string{"SHA256SUMS", "sha256sums.txt", "checksums.txt", "SHA256SUMS.txt", "sha256sum.txt"}

// SelectAsset picks the release asset holding the binary for this platform.
//
// A descriptor's asset template wins outright; otherwise the name has to
// mention both GOOS and GOARCH, which is what a conventional Go release
// already produces. Only an installable shape is considered -- a bare
// executable, or a .tar.gz or .zip -- so checksums, signatures and formats
// Outpost cannot open are skipped. A bare binary is preferred; a .tar.gz or .zip is taken when the
// release ships nothing else, and Install unpacks it.
func (c *Client) SelectAsset(rel *Release, d *Descriptor) (Asset, error) {
	if want := d.assetName(c.GOOS, c.GOARCH); want != "" {
		for _, a := range rel.Assets {
			if a.Name == want {
				// A .tar.gz or .zip is unpacked; anything else that is not a
				// binary would be chmod +x'd and fail later at exec time with
				// an error naming neither the file nor the descriptor.
				// The descriptor's author named this asset, so only what is
				// certainly not a binary is refused; the shape heuristic below
				// is for guessing, and must not overrule them.
				if isChecksumName(a.Name) || isSignature(a.Name) || isUnsupportedArchive(a.Name) {
					return Asset{}, fmt.Errorf("%s names %q, which is not a binary and not an archive Outpost can open "+
						"(.tar.gz and .zip are); name the binary or a supported archive",
						DescriptorName, a.Name)
				}
				return a, nil
			}
		}
		return Asset{}, fmt.Errorf("release %s publishes no asset named %q (from %s)", rel.Tag, want, DescriptorName)
	}

	var platform []Asset
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if !takeable(a.Name, c.GOOS) {
			continue
		}
		if !matchesOS(name, c.GOOS) || !matchesArch(name, c.GOARCH) {
			continue
		}
		platform = append(platform, a)
	}

	var matches, archives []Asset
	for _, a := range platform {
		// "<asset>.<anything>" is a file about that asset -- a .sha512, a
		// .sigstore, a .bundle -- not another build of it. The suffix lists
		// above cannot name every such extension, and one they missed would
		// otherwise count as a bare binary and hide the real archive.
		if isSidecar(a, platform) {
			continue
		}
		if archiveKind(a.Name) != archiveNone {
			archives = append(archives, a)
			continue
		}
		matches = append(matches, a)
	}
	// A bare binary is preferred, but an archive is not a dead end: most
	// releases ship only archives, and refusing them made `outpost add <url>`
	// fail for the majority of real repositories.
	if len(matches) == 0 {
		matches = archives
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Asset{}, &NoPlatformAssetError{Tag: rel.Tag, GOOS: c.GOOS, GOARCH: c.GOARCH}
	}

	// Releases routinely ship variants beside the plain build -- a _gui, a
	// -static, a -musl -- or the same build in two archive formats. They are
	// the same platform, so the platform match cannot separate them, and
	// failing would turn an ordinary release into a dead end.
	if pick, ok := plainest(matches, c.GOOS); ok {
		if len(pick.formats) > 0 {
			c.logf("release %s also publishes the same build as %s; taking %s",
				rel.Tag, strings.Join(pick.formats, ", "), pick.asset.Name)
		}
		if len(pick.variants) > 0 {
			c.logf("release %s also publishes %s; taking the plain build %s",
				rel.Tag, strings.Join(pick.variants, ", "), pick.asset.Name)
		}
		return pick.asset, nil
	}

	names := make([]string, 0, len(matches))
	for _, a := range matches {
		names = append(names, a.Name)
	}
	return Asset{}, fmt.Errorf("release %s publishes %d assets matching %s/%s (%s); "+
		"name the right one with `asset = \"...\"` in %s",
		rel.Tag, len(matches), c.GOOS, c.GOARCH, strings.Join(names, ", "), DescriptorName)
}

// isSidecar reports an asset named as another candidate plus a suffix.
func isSidecar(a Asset, others []Asset) bool {
	for _, o := range others {
		if o.Name != a.Name && strings.HasPrefix(a.Name, o.Name+".") {
			return true
		}
	}
	return false
}

// variantQualifiers are the words releases append to a plain build's name to
// mark a variant of the same program. Anything else after the plain name --
// "-server", "-cli" -- may well be a different program, and choosing between
// programs is not something to guess at.
var variantQualifiers = map[string]bool{
	"gui": true, "nogui": true, "headless": true, "static": true, "musl": true,
	"glibc": true, "gnu": true, "slim": true, "full": true, "lite": true,
	"minimal": true, "pure": true, "cgo": true, "nocgo": true, "baseline": true,
	"upx": true, "compressed": true,
}

// pick is plainest's choice and what it passed over.
type pick struct {
	asset    Asset
	variants []string // qualified builds of the same program
	formats  []string // the same build in another archive format
}

// plainest picks the base build out of a set of variants, and reports which
// ones it passed over.
//
// The test is a prefix plus a known qualifier, not merely a shorter name.
// "yarr_darwin_arm64" is a prefix of "yarr_darwin_arm64_gui", and "gui" is a
// variant word, so the second is the first with a qualifier bolted on.
// "svc-linux-amd64" is not a prefix of "svc-server-linux-amd64", and
// "svc-linux-amd64-server" adds a word that names a program rather than a
// variant; both are differently named programs that happen to share a
// platform. An identical name in two archive formats is one build, and the
// format native to the platform is taken.
func plainest(matches []Asset, goos string) (pick, bool) {
	stem := func(a Asset) string { return strings.ToLower(stripArchiveExt(a.Name)) }

	base := stem(matches[0])
	for _, a := range matches[1:] {
		if s := stem(a); len(s) < len(base) {
			base = s
		}
	}

	var same []Asset
	var variants []string
	for _, a := range matches {
		s := stem(a)
		if s == base {
			same = append(same, a)
			continue
		}
		rest, _ := strings.CutPrefix(s, base)
		if len(rest) == len(s) || !onlyQualifiers(rest) {
			return pick{}, false
		}
		variants = append(variants, a.Name)
	}

	sort.SliceStable(same, func(i, j int) bool {
		return formatRank(same[i].Name, goos) < formatRank(same[j].Name, goos)
	})
	if len(same) > 1 && formatRank(same[0].Name, goos) == formatRank(same[1].Name, goos) {
		// Two assets with the same stem in the same format: nothing to prefer.
		return pick{}, false
	}
	var formats []string
	for _, a := range same[1:] {
		formats = append(formats, a.Name)
	}
	sort.Strings(variants)
	return pick{asset: same[0], variants: variants, formats: formats}, true
}

// onlyQualifiers reports a name tail made only of variant words.
func onlyQualifiers(rest string) bool {
	words := strings.FieldsFunc(rest, isNameSeparator)
	if len(words) == 0 {
		return false
	}
	for _, w := range words {
		if !variantQualifiers[w] {
			return false
		}
	}
	return true
}

// formatRank orders the formats of one build: a bare binary first, then the
// archive native to the platform.
func formatRank(name, goos string) int {
	switch archiveKind(name) {
	case archiveNone:
		return 0
	case archiveZip:
		if goos == "windows" {
			return 1
		}
		return 2
	default:
		if goos == "windows" {
			return 2
		}
		return 1
	}
}

// archOverlaps lists, for each architecture that is a prefix or narrower
// relative of another, the names whose presence means the asset is for the
// wider one. The match below is on word boundaries, which already keeps "arm"
// out of "arm64"; this still matters where the narrower spelling is itself a
// whole word inside the wider one, as "x86" is in "x86_64".
var archOverlaps = map[string][]string{
	"arm":    {"arm64", "armv8", "aarch64"},
	"mips":   {"mips64", "mips64le", "mipsle"},
	"mips64": {"mips64le"},
	"ppc64":  {"ppc64le"},
	"386":    {"x86_64", "x86-64", "amd64"},
}

// goosAliases and goarchAliases are the spellings releases use that are not
// Go's. Plenty of projects ship "macos" or "x86_64"; refusing those made
// `outpost add <url>` fail on naming alone, with an error blaming the platform
// rather than the vocabulary.
var goosAliases = map[string][]string{
	"darwin":  {"macos", "osx", "apple", "mac"},
	"windows": {"win"},
}

// Because matching is on word boundaries, "armv7" and "i386" are not
// redundant with "arm" and "386": neither contains its GOARCH as a whole word.
var goarchAliases = map[string][]string{
	"amd64": {"x86_64", "x86-64", "x64"},
	"arm64": {"aarch64", "armv8"},
	"386":   {"i386", "i686", "x86"},
	"arm":   {"armv7", "armv7l", "armv6", "armhf"},
}

// isNameSeparator splits an asset name into words.
func isNameSeparator(r rune) bool {
	return (r < 'a' || r > 'z') && (r < '0' || r > '9')
}

// hasWord reports word appearing in lowerName with a separator, or either end
// of the name, on both sides. A raw substring match let "win" match inside
// "darwin" and "mac" inside a project called "macchina".
//
// digitsAfter lets the word be followed by digits before the boundary, for the
// OS spellings that carry a bitness: "win64", "linux32", "macos11".
func hasWord(lowerName, word string, digitsAfter bool) bool {
	for from := 0; ; {
		i := strings.Index(lowerName[from:], word)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(word)
		if digitsAfter {
			for end < len(lowerName) && lowerName[end] >= '0' && lowerName[end] <= '9' {
				end++
			}
		}
		before := start == 0 || isNameSeparator(rune(lowerName[start-1]))
		after := end == len(lowerName) || isNameSeparator(rune(lowerName[end]))
		if before && after {
			return true
		}
		from = start + 1
	}
}

// matchesOS matches GOOS and the spellings that mean it, as whole words.
func matchesOS(lowerName, goos string) bool {
	if hasWord(lowerName, goos, true) {
		return true
	}
	for _, alias := range goosAliases[goos] {
		if hasWord(lowerName, alias, true) {
			return true
		}
	}
	return false
}

// matchesArch is the same over GOARCH, refusing names that belong to a wider
// architecture this one is merely a part of.
func matchesArch(lowerName, goarch string) bool {
	for _, wider := range archOverlaps[goarch] {
		if strings.Contains(lowerName, wider) {
			return false
		}
	}
	if hasWord(lowerName, goarch, false) {
		return true
	}
	for _, alias := range goarchAliases[goarch] {
		if hasWord(lowerName, alias, false) {
			return true
		}
	}
	return false
}

// takeable reports an asset SelectAsset may guess at: installable in shape,
// and not a checksum manifest, signature or unopenable archive. installable
// alone misses the extensionless "SHA256SUMS", and the ".7z"-style names that
// read like versions.
func takeable(name, goos string) bool {
	return installable(name, goos) && !isChecksumName(name) && !isSignature(name) && !isUnsupportedArchive(name)
}

// platformWords are the words, Go's and the aliases, that name a platform.
// Built once from the same tables matchesOS and matchesArch read, plus every
// GOOS and GOARCH Go knows that has no alias.
var platformWords = func() map[string]bool {
	words := map[string]bool{}
	for _, w := range []string{
		"aix", "android", "dragonfly", "freebsd", "illumos", "ios", "js", "linux",
		"netbsd", "openbsd", "plan9", "solaris", "wasip1", "wasm", "loong64",
		"mips", "mipsle", "mips64", "mips64le", "ppc64", "ppc64le", "riscv64", "s390x",
	} {
		words[w] = true
	}
	for _, table := range []map[string][]string{goosAliases, goarchAliases} {
		for goName, aliases := range table {
			words[goName] = true
			for _, a := range aliases {
				words[a] = true
			}
		}
	}
	return words
}()

// isPlatformWord reports a word naming a platform, allowing the trailing
// digits matchesOS allows ("win64", "linux32", "macos11").
func isPlatformWord(w string) bool {
	return platformWords[w] || platformWords[strings.TrimRight(w, "0123456789")]
}

// programName is the part of an asset's name before the platform or version
// begins: "plumbline" in "plumbline_1.2.0_linux_amd64.tar.gz". It is a second
// name to look for inside an archive.
//
// The first word always belongs to the program, so names that start like a
// version or a platform -- v2ray, 3proxy, win-acme -- are not cut to nothing.
func programName(asset string) string {
	// ASCII-only lowering and separators keep every offset on a rune
	// boundary of asset, so slicing it cannot split a character.
	sep := func(b byte) bool { return b < 0x80 && isNameSeparator(rune(b)) }
	lower := []byte(asset)
	for i, b := range lower {
		if 'A' <= b && b <= 'Z' {
			lower[i] = b + 'a' - 'A'
		}
	}
	start, word := 0, 0
	for i := 0; i <= len(lower); i++ {
		if i < len(lower) && !sep(lower[i]) {
			continue
		}
		w := string(lower[start:i])
		if w != "" {
			isVersion := w[0] >= '0' && w[0] <= '9' || len(w) > 1 && w[0] == 'v' && w[1] >= '0' && w[1] <= '9'
			if word > 0 && (isPlatformWord(w) || isVersion) {
				end := start
				for end > 0 && sep(lower[end-1]) {
					end--
				}
				return asset[:end]
			}
			word++
		}
		start = i + 1
	}
	return ""
}

// memberNames are the names to look for inside an archive: the service's,
// then the program's the asset is named after.
func memberNames(service, asset string) []string {
	names := []string{service}
	if p := programName(asset); p != "" && !strings.EqualFold(p, service) {
		names = append(names, p)
	}
	return names
}

func isChecksumName(name string) bool {
	lower := strings.ToLower(name)
	for _, n := range checksumNames {
		if lower == strings.ToLower(n) {
			return true
		}
	}
	return hasSuffixAny(lower, checksumExts)
}

// UnverifiedError is an asset with no published checksum, refused because the
// client was told to require one.
type UnverifiedError struct {
	Tag, Asset string
	// Published says the release has a checksum file, just not one listing
	// this asset.
	Published bool
}

func (e *UnverifiedError) Error() string {
	if e.Published {
		return fmt.Sprintf("release %s publishes checksums but none for %s", e.Tag, e.Asset)
	}
	return fmt.Sprintf("release %s publishes no checksums for %s", e.Tag, e.Asset)
}

// CheckVerifiable returns an *UnverifiedError if Install could not verify the
// asset against the release's checksums, so a caller can refuse before doing
// anything Install would otherwise have to undo.
func (c *Client) CheckVerifiable(ctx context.Context, rel *Release, assetName string) error {
	want, published, err := c.expectedSum(ctx, rel, assetName)
	if err != nil {
		return err
	}
	if want == "" {
		return &UnverifiedError{Tag: rel.Tag, Asset: assetName, Published: published}
	}
	return nil
}

// checksumExts are per-asset checksum files. Only the SHA-256 ones are read;
// the rest are recognised so that they are never mistaken for a binary.
var checksumExts = []string{".sha256", ".sha256sum", ".sha512", ".sha512sum", ".sha1", ".sha1sum", ".md5", ".md5sum"}

// Install downloads the asset to dest, verifying it against the release's
// checksums when it publishes any.
//
// Verification is best-effort by design: most releases publish a checksum file
// and those are checked and refused on mismatch, but requiring one would turn
// away otherwise fine services and push the operator back to --binary, which
// verifies nothing at all. When there is nothing to check against, the result
// says so rather than implying the download was validated.
func (c *Client) Install(ctx context.Context, rel *Release, asset Asset, dest string) (Result, error) {
	res := Result{Tag: rel.Tag, Asset: asset.Name, Path: dest}

	want, published, err := c.expectedSum(ctx, rel, asset.Name)
	if err != nil {
		return res, err
	}
	// Refused before downloading, so nothing at dest has been touched.
	if want == "" && c.RequireChecksum {
		return res, &UnverifiedError{Tag: rel.Tag, Asset: asset.Name, Published: published}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", filepath.Dir(dest), err)
	}
	// Download beside the target and rename, so a failure part-way through
	// cannot leave a truncated binary where a working one used to be.
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".outpost-download-*")
	if err != nil {
		return res, fmt.Errorf("create a temporary file next to %s: %w", dest, err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()

	sum, err := c.download(ctx, asset.downloadURL(), tmp)
	if err != nil {
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, fmt.Errorf("write %s: %w", tmpName, err)
	}

	switch {
	case want == "" && published:
		// A checksum file exists but does not list this asset — a different
		// failure from publishing none, and saying "no checksums" would send
		// the operator looking for a file that is right there.
		res.Warning = fmt.Sprintf("release %s publishes checksums but none for %s; it was installed unverified", rel.Tag, asset.Name)
	case want == "":
		res.Warning = fmt.Sprintf("release %s publishes no checksums; %s was installed unverified", rel.Tag, asset.Name)
	case !strings.EqualFold(want, sum):
		return res, fmt.Errorf("checksum mismatch for %s: the release lists %s, the download is %s; refusing to install",
			asset.Name, want, sum)
	default:
		res.Verified = true
	}

	// The checksum above covers what the release published. For an archive
	// that is the archive itself, which is the right thing to verify: the
	// binary is only trusted because the container it came out of was.
	install := tmpName
	if kind := archiveKind(asset.Name); kind != archiveNone {
		extracted, err := os.CreateTemp(filepath.Dir(dest), ".outpost-extract-*")
		if err != nil {
			return res, fmt.Errorf("create a temporary file next to %s: %w", dest, err)
		}
		extractedName := extracted.Name()
		// Only the name was wanted; extraction reopens or replaces the file,
		// and any write failure surfaces there.
		_ = extracted.Close()
		defer os.Remove(extractedName)

		member, err := extractMember(ctx, tmpName, extractedName, kind, c.GOOS, memberNames(filepath.Base(dest), asset.Name))
		if err != nil {
			return res, fmt.Errorf("%s: %w", asset.Name, err)
		}
		res.Member = member
		install = extractedName
	}

	if err := os.Chmod(install, 0o755); err != nil {
		return res, fmt.Errorf("chmod %s: %w", install, err)
	}
	if err := os.Rename(install, dest); err != nil {
		return res, fmt.Errorf("install %s: %w", dest, err)
	}
	return res, nil
}

// downloadText fetches a small text file — a checksum manifest or a
// descriptor — into memory. It gets its own, far smaller bound: an asset may
// legitimately be hundreds of megabytes, but buffering that much of a
// checksum file in a strings.Builder never is.
func (c *Client) downloadText(ctx context.Context, url string, w io.Writer) (string, error) {
	return c.downloadLimit(ctx, url, w, maxTextBytes)
}

// download copies the body to w and returns its SHA-256.
func (c *Client) download(ctx context.Context, url string, w io.Writer) (string, error) {
	return c.downloadLimit(ctx, url, w, maxAssetBytes)
}

func (c *Client) downloadLimit(ctx context.Context, url string, w io.Writer, max int64) (string, error) {
	resp, err := c.get(ctx, url, "application/octet-stream")
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", url, resp.Status)
	}

	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, sum), io.LimitReader(resp.Body, max+1))
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	if n > max {
		return "", fmt.Errorf("download %s: larger than the %d MiB limit", url, max>>20)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// expectedSum finds the published SHA-256 for one asset. It also reports
// whether the release published any checksum file at all, so the caller can
// tell "nothing to check against" from "checksums exist but not for this
// asset" — two different things to warn about.
//
// Both shapes are tried, and a release-wide manifest that turns out not to
// list this asset falls through to the per-asset one rather than reporting
// "no checksums" while a usable "<name>.sha256" sits in the same release.
func (c *Client) expectedSum(ctx context.Context, rel *Release, assetName string) (string, bool, error) {
	// perAsset travels with each candidate: a lone hash means "this asset"
	// only in a "<asset>.sha256", never in a release-wide manifest.
	type candidate struct {
		asset    *Asset
		perAsset bool
	}
	var candidates []candidate
	// Several checksumNames entries differ only in case, so the same asset can
	// answer more than one of them; seen keeps it from being fetched twice.
	seen := map[int]bool{}
	for _, want := range checksumNames {
		for i, a := range rel.Assets {
			if strings.EqualFold(a.Name, want) {
				if !seen[i] {
					seen[i] = true
					candidates = append(candidates, candidate{&rel.Assets[i], false})
				}
				break
			}
		}
	}
	// A per-asset "<name>.sha256" is the other common shape; ".sha256sum" is
	// the same file under the other conventional suffix, which isChecksumName
	// already recognises.
	for _, suffix := range []string{".sha256", ".sha256sum"} {
		for i, a := range rel.Assets {
			if strings.EqualFold(a.Name, assetName+suffix) {
				if !seen[i] {
					seen[i] = true
					candidates = append(candidates, candidate{&rel.Assets[i], true})
				}
				break
			}
		}
	}

	for _, cand := range candidates {
		var buf strings.Builder
		if _, err := c.downloadText(ctx, cand.asset.downloadURL(), &buf); err != nil {
			return "", true, fmt.Errorf("fetch %s: %w", cand.asset.Name, err)
		}
		if sum := parseChecksumsScoped(buf.String(), assetName, cand.perAsset); sum != "" {
			return sum, true, nil
		}
	}
	return "", len(candidates) > 0, nil
}

// parseChecksums reads a per-asset checksum file, where a lone hash with no
// filename means this asset.
func parseChecksums(body, assetName string) string {
	return parseChecksumsScoped(body, assetName, true)
}

// parseChecksumsScoped reads `sha256sum` output: "<hex>  <name>" per line.
//
// perAsset says whether a lone hash with no filename may be taken as this
// asset's. That is true of a "<asset>.sha256", which is what `sha256sum <
// file` produces. It is not true of a release-wide SHA256SUMS: a single bare
// hash there belongs to whatever the publisher was checksumming, and reading
// it as this asset's would refuse a perfectly good binary for a mismatch
// against an unrelated artifact.
func parseChecksumsScoped(body, assetName string, perAsset bool) string {
	var loneHash string
	var lines int
	for _, line := range strings.Split(body, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		lines++
		if len(fields) == 1 {
			loneHash = fields[0]
			continue
		}
		// The name may carry sha256sum's binary-mode "*" prefix, or a path.
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if filepath.Base(name) == assetName {
			return fields[0]
		}
	}
	if perAsset && lines == 1 && loneHash != "" {
		return loneHash
	}
	return ""
}

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
// already produces. Checksum manifests and archives are skipped: this installs
// a binary, and silently handing someone a .tar.gz named as one would fail
// later and less clearly.
func (c *Client) SelectAsset(rel *Release, d *Descriptor) (Asset, error) {
	if want := d.assetName(c.GOOS, c.GOARCH); want != "" {
		for _, a := range rel.Assets {
			if a.Name == want {
				// A .tar.gz or .zip is unpacked; anything else that is not a
				// binary would be chmod +x'd and fail later at exec time with
				// an error naming neither the file nor the descriptor.
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

	var matches, archives []Asset
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if isChecksumName(a.Name) || isSignature(a.Name) || isUnsupportedArchive(a.Name) {
			continue
		}
		if !matchesOS(name, c.GOOS) || !matchesArch(name, c.GOARCH) {
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
		return Asset{}, fmt.Errorf("release %s publishes no asset for %s/%s; "+
			"name one with `asset = \"...${GOOS}-${GOARCH}\"` in %s, or use --binary",
			rel.Tag, c.GOOS, c.GOARCH, DescriptorName)
	}

	// Releases routinely ship variants beside the plain build -- a _gui, a
	// -static, a -musl. They are the same platform, so the platform match
	// cannot separate them, and failing would turn an ordinary release into a
	// dead end.
	if plain, others, ok := plainest(matches); ok {
		c.logf("release %s also publishes %s; taking the plain build %s",
			rel.Tag, strings.Join(others, ", "), plain.Name)
		return plain, nil
	}

	names := make([]string, 0, len(matches))
	for _, a := range matches {
		names = append(names, a.Name)
	}
	return Asset{}, fmt.Errorf("release %s publishes %d assets matching %s/%s (%s); "+
		"name the right one with `asset = \"...\"` in %s",
		rel.Tag, len(matches), c.GOOS, c.GOARCH, strings.Join(names, ", "), DescriptorName)
}

// plainest picks the base build out of a set of variants, and reports which
// ones it passed over.
//
// The test is a prefix, not merely a shorter name. "yarr_darwin_arm64" is a
// prefix of "yarr_darwin_arm64_gui", so the second is the first with a
// qualifier bolted on -- a variant. "svc-linux-amd64" is not a prefix of
// "svc-server-linux-amd64", which is a differently named program that happens
// to share a platform, and choosing between those two is not something to
// guess at. Length alone would have conflated them.
func plainest(matches []Asset) (Asset, []string, bool) {
	best := matches[0]
	for _, a := range matches[1:] {
		if len(a.Name) < len(best.Name) {
			best = a
		}
	}
	stem := strings.ToLower(stripArchiveExt(best.Name))

	others := make([]string, 0, len(matches)-1)
	for _, a := range matches {
		if a.Name == best.Name {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(stripArchiveExt(a.Name)), stem) {
			return Asset{}, nil, false
		}
		others = append(others, a.Name)
	}
	if len(others) == 0 {
		return Asset{}, nil, false
	}
	sort.Strings(others)
	return best, others, true
}

// stripArchiveExt drops a recognised archive suffix so two names can be
// compared on what they actually say.
func stripArchiveExt(name string) string {
	lower := strings.ToLower(name)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip"} {
		if strings.HasSuffix(lower, ext) {
			return name[:len(name)-len(ext)]
		}
	}
	return name
}

// archOverlaps lists, for each architecture that is a prefix or narrower
// relative of another, the names whose presence means the asset is for the
// wider one. "arm64" contains "arm", so a plain Contains would hand a 32-bit
// ARM host an arm64 binary that cannot exec at all; ppc64/ppc64le, the mips
// family and x86/x86_64 have the same shape.
var archOverlaps = map[string][]string{
	"arm":    {"arm64", "armv8", "aarch64"},
	"mips":   {"mips64", "mips64le", "mipsle"},
	"mips64": {"mips64le"},
	"ppc64":  {"ppc64le"},
	"386":    {"x86_64", "amd64"},
}

// goosAliases and goarchAliases are the spellings releases use that are not
// Go's. Plenty of projects ship "macos" or "x86_64"; refusing those made
// `outpost add <url>` fail on naming alone, with an error blaming the platform
// rather than the vocabulary.
var goosAliases = map[string][]string{
	"darwin":  {"macos", "osx", "apple", "mac"},
	"windows": {"win"},
}

var goarchAliases = map[string][]string{
	"amd64": {"x86_64", "x64"},
	"arm64": {"aarch64", "armv8"},
	"386":   {"i386", "x86"},
	"arm":   {"armv7", "armv6", "armhf"},
}

// matchesOS is a substring match over GOOS and the spellings that mean it.
func matchesOS(lowerName, goos string) bool {
	if strings.Contains(lowerName, goos) {
		return true
	}
	for _, alias := range goosAliases[goos] {
		if strings.Contains(lowerName, alias) {
			return true
		}
	}
	return false
}

// matchesArch is the same over GOARCH, refusing names that belong to a wider
// architecture this one is merely a substring of.
func matchesArch(lowerName, goarch string) bool {
	for _, wider := range archOverlaps[goarch] {
		if strings.Contains(lowerName, wider) {
			return false
		}
	}
	if strings.Contains(lowerName, goarch) {
		return true
	}
	for _, alias := range goarchAliases[goarch] {
		if strings.Contains(lowerName, alias) {
			return true
		}
	}
	return false
}

func isChecksumName(name string) bool {
	lower := strings.ToLower(name)
	for _, n := range checksumNames {
		if lower == strings.ToLower(n) {
			return true
		}
	}
	return strings.HasSuffix(lower, ".sha256") || strings.HasSuffix(lower, ".sha256sum")
}

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
		extracted.Close()
		defer os.Remove(extractedName)

		member, err := extractBinary(tmpName, extractedName, filepath.Base(dest), kind)
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

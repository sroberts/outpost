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
	"strings"
)

// Result describes what was installed.
type Result struct {
	Tag   string
	Asset string
	Path  string
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
				// Nothing here unpacks an archive, so installing one as the
				// binary would chmod +x a tarball and fail at exec time with
				// an error naming neither the archive nor the descriptor.
				if hasArchiveExt(strings.ToLower(a.Name)) {
					return Asset{}, fmt.Errorf("%s names %q, which is an archive; Outpost installs a bare binary and does not unpack archives",
						DescriptorName, a.Name)
				}
				return a, nil
			}
		}
		return Asset{}, fmt.Errorf("release %s publishes no asset named %q (from %s)", rel.Tag, want, DescriptorName)
	}

	var matches []Asset
	for _, a := range rel.Assets {
		name := strings.ToLower(a.Name)
		if isChecksumName(a.Name) || hasArchiveExt(name) {
			continue
		}
		if strings.Contains(name, c.GOOS) && matchesArch(name, c.GOARCH) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return Asset{}, fmt.Errorf("release %s publishes no asset for %s/%s; "+
			"name one with `asset = \"...${GOOS}-${GOARCH}\"` in %s, or use --binary",
			rel.Tag, c.GOOS, c.GOARCH, DescriptorName)
	default:
		names := make([]string, 0, len(matches))
		for _, a := range matches {
			names = append(names, a.Name)
		}
		return Asset{}, fmt.Errorf("release %s publishes %d assets matching %s/%s (%s); "+
			"name the right one with `asset = \"...\"` in %s",
			rel.Tag, len(matches), c.GOOS, c.GOARCH, strings.Join(names, ", "), DescriptorName)
	}
}

// matchesArch is a substring match with the one overlap that matters spelled
// out: "arm64" contains "arm", so a plain Contains would hand a 32-bit ARM
// host (GOARCH=arm) an arm64 binary that cannot exec at all. Every other
// GOARCH value is a prefix of nothing else Go emits.
func matchesArch(lowerName, goarch string) bool {
	if !strings.Contains(lowerName, goarch) {
		return false
	}
	if goarch == "arm" {
		return !strings.Contains(lowerName, "arm64") && !strings.Contains(lowerName, "armv8")
	}
	return true
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

func hasArchiveExt(lower string) bool {
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar.xz", ".tar.bz2", ".deb", ".rpm", ".sig", ".pem", ".asc"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
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

	if err := os.Chmod(tmpName, 0o755); err != nil {
		return res, fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
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
	var candidates []*Asset
	// Several checksumNames entries differ only in case, so the same asset can
	// answer more than one of them; seen keeps it from being fetched twice.
	seen := map[int]bool{}
	for _, want := range checksumNames {
		for i, a := range rel.Assets {
			if strings.EqualFold(a.Name, want) {
				if !seen[i] {
					seen[i] = true
					candidates = append(candidates, &rel.Assets[i])
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
					candidates = append(candidates, &rel.Assets[i])
				}
				break
			}
		}
	}

	for _, manifest := range candidates {
		var buf strings.Builder
		if _, err := c.downloadText(ctx, manifest.downloadURL(), &buf); err != nil {
			return "", true, fmt.Errorf("fetch %s: %w", manifest.Name, err)
		}
		if sum := parseChecksums(buf.String(), assetName); sum != "" {
			return sum, true, nil
		}
	}
	return "", len(candidates) > 0, nil
}

// parseChecksums reads `sha256sum` output: "<hex>  <name>" per line. A
// single-entry file with no name is treated as being about the asset itself,
// which is what a "<asset>.sha256" usually contains.
func parseChecksums(body, assetName string) string {
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
	if lines == 1 && loneHash != "" {
		return loneHash
	}
	return ""
}

package acquire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHub is a stand-in for the GitHub API and its asset host, so none of
// these tests touch the network.
type fakeHub struct {
	t          *testing.T
	srv        *httptest.Server
	release    *Release
	files      map[string][]byte // asset name -> body
	descriptor []byte            // served from the tagged tree; nil means 404
	status     int               // overrides the release response status
	authSeen   string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	h := &fakeHub{t: t, files: map[string][]byte{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		h.authSeen = r.Header.Get("Authorization")
		if h.status != 0 {
			w.WriteHeader(h.status)
			return
		}
		_ = json.NewEncoder(w).Encode(h.release)
	})
	mux.HandleFunc("/repos/o/r/contents/outpost.toml", func(w http.ResponseWriter, r *http.Request) {
		if h.descriptor == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(h.descriptor)
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		body, ok := h.files[strings.TrimPrefix(r.URL.Path, "/dl/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write(body)
	})

	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// addAsset publishes an asset and returns its recorded metadata.
func (h *fakeHub) addAsset(name string, body []byte) {
	h.files[name] = body
	h.release.Assets = append(h.release.Assets, Asset{
		Name: name,
		URL:  h.srv.URL + "/dl/" + name,
		Size: int64(len(body)),
	})
}

func (h *fakeHub) client() *Client {
	return &Client{
		HTTP:    h.srv.Client(),
		BaseURL: h.srv.URL,
		GOOS:    "linux",
		GOARCH:  "amd64",
	}
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func hubWithBinary(t *testing.T, body []byte, checksums bool) (*fakeHub, *Client) {
	t.Helper()
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1.0.0"}
	h.addAsset("svc-linux-amd64", body)
	h.addAsset("svc-darwin-arm64", []byte("other platform"))
	if checksums {
		h.addAsset("SHA256SUMS", []byte(fmt.Sprintf("%s  svc-linux-amd64\n%s  svc-darwin-arm64\n",
			sum(body), sum([]byte("other platform")))))
	}
	return h, h.client()
}

var repo = Repo{Owner: "o", Name: "r"}

func TestLatestRelease_ReadsTheTagAndAssets(t *testing.T) {
	h, c := hubWithBinary(t, []byte("binary"), true)

	rel, err := c.LatestRelease(context.Background(), repo)
	if err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if rel.Tag != "v1.0.0" {
		t.Errorf("Tag = %q; want v1.0.0", rel.Tag)
	}
	if len(rel.Assets) != 3 {
		t.Errorf("got %d assets; want 3", len(rel.Assets))
	}
	if h.authSeen != "" {
		t.Errorf("sent an Authorization header with no token: %q", h.authSeen)
	}
}

// A token must actually reach GitHub, or private repositories silently fail.
func TestLatestRelease_SendsTheToken(t *testing.T) {
	h, c := hubWithBinary(t, []byte("binary"), false)
	c.Token = "gho_secret"

	if _, err := c.LatestRelease(context.Background(), repo); err != nil {
		t.Fatalf("LatestRelease: %v", err)
	}
	if h.authSeen != "Bearer gho_secret" {
		t.Errorf("Authorization = %q; want the bearer token", h.authSeen)
	}
}

// 404 covers both "no release" and "private, and you are not authenticated",
// and the operator needs to know which one they are looking at.
func TestLatestRelease_MissingSuggestsAuthWhenUnauthenticated(t *testing.T) {
	h, c := hubWithBinary(t, []byte("binary"), false)
	h.status = http.StatusNotFound

	_, err := c.LatestRelease(context.Background(), repo)
	if err == nil {
		t.Fatal("want an error for a 404")
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("error does not mention authenticating: %v", err)
	}

	c.Token = "gho_secret"
	_, err = c.LatestRelease(context.Background(), repo)
	if err == nil {
		t.Fatal("want an error for a 404")
	}
	if strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("suggested authenticating to an already-authenticated caller: %v", err)
	}
}

func TestSelectAsset_MatchesThePlatform(t *testing.T) {
	_, c := hubWithBinary(t, []byte("binary"), true)
	rel, _ := c.LatestRelease(context.Background(), repo)

	got, err := c.SelectAsset(rel, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "svc-linux-amd64" {
		t.Errorf("selected %q; want the linux/amd64 asset", got.Name)
	}
}

// The checksum manifest mentions every platform, so a naive substring match
// would pick it as the binary.
func TestSelectAsset_SkipsChecksumsAndArchives(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("SHA256SUMS", []byte("whatever"))
	h.addAsset("svc-linux-amd64.tar.gz", []byte("archive"))
	h.addAsset("svc-linux-amd64.sha256", []byte("hash"))
	h.addAsset("svc-linux-amd64", []byte("binary"))

	got, err := h.client().SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "svc-linux-amd64" {
		t.Errorf("selected %q; want the bare binary", got.Name)
	}
}

func TestSelectAsset_AmbiguityIsAnErrorThatSaysWhatToDo(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", []byte("a"))
	h.addAsset("svc-server-linux-amd64", []byte("b"))

	_, err := h.client().SelectAsset(h.release, &Descriptor{})
	if err == nil {
		t.Fatal("two matching assets: want an error")
	}
	if !strings.Contains(err.Error(), "asset =") {
		t.Errorf("error does not say how to disambiguate: %v", err)
	}
}

func TestSelectAsset_DescriptorTemplateWins(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", []byte("a"))
	h.addAsset("custom_linux_amd64_bin", []byte("b"))

	got, err := h.client().SelectAsset(h.release, &Descriptor{Asset: "custom_${GOOS}_${GOARCH}_bin"})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "custom_linux_amd64_bin" {
		t.Errorf("selected %q; want the descriptor's asset", got.Name)
	}
}

func TestInstall_VerifiesAgainstThePublishedChecksum(t *testing.T) {
	body := []byte("#!/bin/sh\necho hi\n")
	_, c := hubWithBinary(t, body, true)
	rel, _ := c.LatestRelease(context.Background(), repo)
	asset, _ := c.SelectAsset(rel, &Descriptor{})

	dest := filepath.Join(t.TempDir(), "bin", "svc")
	res, err := c.Install(context.Background(), rel, asset, dest)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Verified {
		t.Error("Verified = false; the release published a matching checksum")
	}
	if res.Warning != "" {
		t.Errorf("Warning = %q; want none", res.Warning)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Errorf("installed content = %q; want the asset body", got)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("mode = %#o; the binary is not executable", fi.Mode().Perm())
	}
}

// The whole point of checking: a mismatch must stop the install, not warn.
func TestInstall_RefusesOnChecksumMismatch(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", []byte("the real binary"))
	h.addAsset("SHA256SUMS", []byte(sum([]byte("something else"))+"  svc-linux-amd64\n"))
	c := h.client()
	asset, _ := c.SelectAsset(h.release, &Descriptor{})

	dest := filepath.Join(t.TempDir(), "svc")
	_, err := c.Install(context.Background(), h.release, asset, dest)
	if err == nil {
		t.Fatal("checksum mismatch: want an error")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error = %v; want it to name the mismatch", err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("a binary was installed despite the mismatch")
	}
}

// No checksums is allowed, but must never be reported as verified.
func TestInstall_UnverifiedWhenNoChecksumsArePublished(t *testing.T) {
	_, c := hubWithBinary(t, []byte("binary"), false)
	rel, _ := c.LatestRelease(context.Background(), repo)
	asset, _ := c.SelectAsset(rel, &Descriptor{})

	res, err := c.Install(context.Background(), rel, asset, filepath.Join(t.TempDir(), "svc"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Verified {
		t.Error("Verified = true with no checksums published")
	}
	if !strings.Contains(res.Warning, "unverified") {
		t.Errorf("Warning = %q; want it to say the download was unverified", res.Warning)
	}
}

func TestDescriptor_PrefersTheReleasesOwnCopy(t *testing.T) {
	h, c := hubWithBinary(t, []byte("binary"), false)
	h.addAsset("outpost.toml", []byte(`name = "from-release"`))
	h.descriptor = []byte(`name = "from-tree"`)
	rel, _ := c.LatestRelease(context.Background(), repo)

	d, err := c.Descriptor(context.Background(), repo, rel)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if d.Name != "from-release" {
		t.Errorf("Name = %q; want the release's own descriptor", d.Name)
	}
}

func TestDescriptor_FallsBackToTheTaggedTree(t *testing.T) {
	h, c := hubWithBinary(t, []byte("binary"), false)
	h.descriptor = []byte(`name = "from-tree"`)
	rel, _ := c.LatestRelease(context.Background(), repo)

	d, err := c.Descriptor(context.Background(), repo, rel)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if d.Name != "from-tree" {
		t.Errorf("Name = %q; want the tree's descriptor", d.Name)
	}
}

// No descriptor is a normal case, not a failure: add falls back to flags.
func TestDescriptor_AbsentIsNotAnError(t *testing.T) {
	_, c := hubWithBinary(t, []byte("binary"), false)
	rel, _ := c.LatestRelease(context.Background(), repo)

	d, err := c.Descriptor(context.Background(), repo, rel)
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if d != nil {
		t.Errorf("Descriptor = %+v; want nil when the repository publishes none", d)
	}
}

// A release-wide manifest that does not happen to list this asset must not end
// the search: giving up on the first candidate reported "no checksums
// published" while a usable per-asset hash sat in the same release.
func TestInstall_FallsThroughToAPerAssetChecksum(t *testing.T) {
	body := []byte("the binary")
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", body)
	// Lists only the archives, as a release publishing both shapes often does.
	h.addAsset("SHA256SUMS", []byte(sum([]byte("tarball"))+"  svc-linux-amd64.tar.gz\n"))
	h.addAsset("svc-linux-amd64.sha256", []byte(sum(body)+"  svc-linux-amd64\n"))

	c := h.client()
	asset, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	res, err := c.Install(context.Background(), h.release, asset, filepath.Join(t.TempDir(), "svc"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Verified {
		t.Errorf("Verified = false; the per-asset checksum should have been used (warning: %q)", res.Warning)
	}
}

// A text file is read into memory, so it gets a much smaller bound than an
// asset streamed to disk.
func TestDownloadText_IsBoundedWellBelowTheAssetLimit(t *testing.T) {
	if maxTextBytes >= maxAssetBytes {
		t.Fatalf("maxTextBytes %d is not below maxAssetBytes %d", maxTextBytes, maxAssetBytes)
	}
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("SHA256SUMS", make([]byte, maxTextBytes+1))

	var buf strings.Builder
	_, err := h.client().downloadText(context.Background(), h.release.Assets[0].URL, &buf)
	if err == nil {
		t.Fatal("an oversized text file was accepted")
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error = %v; want it to name the limit", err)
	}
}

// "arm64" contains "arm", so a plain substring match hands a 32-bit ARM host a
// binary it cannot exec at all — the failure mode being an immediate exec
// error, which the supervisor would then restart-loop.
func TestSelectAsset_ArmDoesNotMatchArm64(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-arm64", []byte("64-bit"))
	h.addAsset("svc-linux-armv8", []byte("also 64-bit"))
	h.addAsset("svc-linux-arm", []byte("32-bit"))

	c := h.client()
	c.GOARCH = "arm"
	got, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got.Name != "svc-linux-arm" {
		t.Errorf("selected %q for GOARCH=arm; want the 32-bit asset", got.Name)
	}

	// A release with only 64-bit ARM assets must refuse, not hand over one.
	h2 := newFakeHub(t)
	h2.release = &Release{Tag: "v1"}
	h2.addAsset("svc-linux-arm64", []byte("64-bit"))
	c2 := h2.client()
	c2.GOARCH = "arm"
	if got, err := c2.SelectAsset(h2.release, &Descriptor{}); err == nil {
		t.Errorf("selected %q for GOARCH=arm; want no match", got.Name)
	}

	// And arm64 must still find its own asset.
	c3 := h.client()
	c3.GOARCH = "arm64"
	if got, err := c3.SelectAsset(h.release, &Descriptor{}); err == nil && got.Name != "svc-linux-arm64" {
		t.Errorf("GOARCH=arm64 selected %q", got.Name)
	}
}

// "checksums exist but not for this asset" is a different problem from
// "publishes none", and sending the operator to look for a file that is right
// there wastes their time.
func TestInstall_DistinguishesMissingChecksumFromMissingEntry(t *testing.T) {
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", []byte("binary"))
	h.addAsset("SHA256SUMS", []byte(sum([]byte("x"))+"  some-other-file\n"))

	c := h.client()
	asset, _ := c.SelectAsset(h.release, &Descriptor{})
	res, err := c.Install(context.Background(), h.release, asset, filepath.Join(t.TempDir(), "svc"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Verified {
		t.Error("Verified = true with no entry for this asset")
	}
	if !strings.Contains(res.Warning, "publishes checksums but none for") {
		t.Errorf("Warning = %q; want it to say the file exists but omits this asset", res.Warning)
	}
	if !strings.Contains(res.Warning, "unverified") {
		t.Errorf("Warning = %q; must still say unverified", res.Warning)
	}
}

// isChecksumName already skipped ".sha256sum" during selection, so a release
// using that suffix had its checksum ignored rather than read.
func TestInstall_ReadsTheSha256sumSuffixToo(t *testing.T) {
	body := []byte("the binary")
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", body)
	h.addAsset("svc-linux-amd64.sha256sum", []byte(sum(body)+"  svc-linux-amd64\n"))

	c := h.client()
	asset, err := c.SelectAsset(h.release, &Descriptor{})
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	res, err := c.Install(context.Background(), h.release, asset, filepath.Join(t.TempDir(), "svc"))
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Verified {
		t.Errorf("Verified = false; the .sha256sum file should have been read (warning: %q)", res.Warning)
	}
}

// A private repository's asset must be fetched from the API endpoint, which
// accepts the bearer token; browser_download_url is served by github.com and
// 404s for a private release.
func TestInstall_PrefersTheAPIAssetURL(t *testing.T) {
	body := []byte("private binary")
	h := newFakeHub(t)
	h.release = &Release{Tag: "v1"}
	h.addAsset("svc-linux-amd64", body)

	// Point browser_download_url at a path that is not served, so only the API
	// URL can succeed.
	h.release.Assets[0].APIURL = h.release.Assets[0].URL
	h.release.Assets[0].URL = h.srv.URL + "/dl/does-not-exist"

	c := h.client()
	res, err := c.Install(context.Background(), h.release, h.release.Assets[0], filepath.Join(t.TempDir(), "svc"))
	if err != nil {
		t.Fatalf("Install used the wrong URL: %v", err)
	}
	if res.Asset != "svc-linux-amd64" {
		t.Errorf("Asset = %q", res.Asset)
	}
}

package acquire

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// maxAssetBytes bounds a download. A supervised service is a single static
// binary; anything past this is not one, and refusing beats filling the disk.
const maxAssetBytes = 512 << 20

// maxTextBytes bounds a file read into memory: a checksum manifest or a
// descriptor. Neither is ever large, and both are buffered rather than
// streamed to disk.
const maxTextBytes = 1 << 20

// DefaultBaseURL is GitHub's REST API.
const DefaultBaseURL = "https://api.github.com"

// Client fetches releases. The zero value is not usable; call NewClient.
type Client struct {
	HTTP    *http.Client
	BaseURL string
	// Token authenticates to the API. Empty works for public repositories.
	Token string
	// GOOS and GOARCH select the asset; overridable so the selection logic is
	// testable for platforms other than the one running the tests.
	GOOS, GOARCH string
	// Logf reports a choice the operator did not make explicitly, such as
	// picking the plain build over a variant. Nil discards.
	Logf func(format string, args ...any)
	// RequireChecksum makes Install refuse an asset the release publishes no
	// checksum for, instead of installing it with a warning. An update sets it
	// when the release it replaces was verified.
	RequireChecksum bool
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

// NewClient returns a client for the host platform, authenticated with the gh
// CLI's token when one is available.
func NewClient() *Client {
	return &Client{
		// No blanket Client.Timeout: it bounds the whole exchange including
		// the body, so a large asset on a slow link would be cut off mid
		// download with no way to extend it, contradicting both the 512 MiB
		// allowance and the caller's own deadline. The per-operation context
		// addFromRepo sets is the real bound; these cover the parts that
		// should be quick regardless of how big the download is.
		HTTP: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
			},
		},
		BaseURL: DefaultBaseURL,
		Token:   ghToken(),
		GOOS:    runtime.GOOS,
		GOARCH:  runtime.GOARCH,
	}
}

// ghToken borrows the gh CLI's credentials rather than storing any of its own,
// which is what makes a private repository work without Outpost ever holding a
// secret. Absent gh, or a logged-out one, is not an error: public repositories
// need no token.
func ghToken() string {
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t
	}
	gh, err := exec.LookPath("gh")
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, gh, "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Asset is one file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	// APIURL is the release-asset endpoint. It is what makes a private
	// repository work: browser_download_url is served by github.com, which
	// does not accept an API token (and Go strips Authorization across the
	// redirect anyway), so a private asset 404s there. The API URL accepts
	// the bearer token and redirects to a pre-signed storage URL.
	APIURL string `json:"url"`
	Size   int64  `json:"size"`
}

// downloadURL is the URL to fetch this asset from, preferring the API
// endpoint when the release listing gave us one.
func (a Asset) downloadURL() string {
	if a.APIURL != "" {
		return a.APIURL
	}
	return a.URL
}

// Release is a published GitHub release.
type Release struct {
	Tag        string  `json:"tag_name"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

func (c *Client) get(ctx context.Context, url string, accept string) (*http.Response, error) {
	return c.getIfChanged(ctx, url, accept, "")
}

// getIfChanged is get with an If-None-Match, when there is an etag to send.
func (c *Client) getIfChanged(ctx context.Context, url, accept, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	return c.HTTP.Do(req)
}

// LatestRelease returns the repository's most recent non-prerelease release.
func (c *Client) LatestRelease(ctx context.Context, repo Repo) (*Release, error) {
	rel, _, err := c.LatestReleaseSince(ctx, repo, "")
	return rel, err
}

// ErrNotModified says the latest release is still the one an etag was given for.
var ErrNotModified = errors.New("the latest release has not changed")

// LatestReleaseSince is LatestRelease for something that asks again and again:
// given the etag from last time it returns ErrNotModified if nothing changed.
// GitHub doesn't count that answer against the rate limit of a client with a
// token; without one it still does, so this saves those only the response. The
// etag comes back with a NoAssetsError too, since that is an answer worth keeping.
func (c *Client) LatestReleaseSince(ctx context.Context, repo Repo, etag string) (*Release, string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", strings.TrimSuffix(c.BaseURL, "/"), repo.Owner, repo.Name)
	resp, err := c.getIfChanged(ctx, url, "application/vnd.github+json", etag)
	if err != nil {
		return nil, "", fmt.Errorf("query releases for %s: %w", repo, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotModified:
		return nil, etag, ErrNotModified
	case http.StatusNotFound:
		// The same status covers both, and the distinction is what the
		// operator needs in order to know what to do next.
		if c.Token == "" {
			return nil, "", fmt.Errorf("%s has no published release, or is private and no credentials were found "+
				"(run `gh auth login`, or set GITHUB_TOKEN)", repo)
		}
		// Authenticated, so this is not a credentials problem — but GitHub
		// returns the same 404 for a repository that does not exist, one the
		// token cannot see, and one with no release at all.
		return nil, "", fmt.Errorf("%s has no published release, does not exist, or is not visible to your token", repo)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, "", fmt.Errorf("%s: GitHub refused the request (%s); the token may be expired or lack access", repo, resp.Status)
	default:
		return nil, "", fmt.Errorf("%s: GitHub returned %s", repo, resp.Status)
	}
	rel, err := decodeRelease(resp.Body, repo)
	return rel, resp.Header.Get("ETag"), err
}

// ReleaseByTag returns the release with the given tag. Unlike LatestRelease it
// takes a prerelease too: an operator who names a tag has chosen it.
func (c *Client) ReleaseByTag(ctx context.Context, repo Repo, tag string) (*Release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s",
		strings.TrimSuffix(c.BaseURL, "/"), repo.Owner, repo.Name, neturl.PathEscape(tag))
	resp, err := c.get(ctx, url, "application/vnd.github+json")
	if err != nil {
		return nil, fmt.Errorf("query release %s of %s: %w", tag, repo, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%s has no release tagged %q, or it is not visible with the credentials found", repo, tag)
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%s: GitHub refused the request (%s); the token may be expired or lack access", repo, resp.Status)
	default:
		return nil, fmt.Errorf("%s: GitHub returned %s", repo, resp.Status)
	}
	return decodeRelease(resp.Body, repo)
}

func decodeRelease(body io.Reader, repo Repo) (*Release, error) {
	var rel Release
	if err := json.NewDecoder(io.LimitReader(body, 4<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode release for %s: %w", repo, err)
	}
	if len(rel.Assets) == 0 {
		return nil, &NoAssetsError{Repo: repo, Tag: rel.Tag}
	}
	return &rel, nil
}

// NoAssetsError is a release with nothing attached to install. For `add` that
// usually means the project does not publish binaries; for an update it more
// often means the release's build has not finished uploading them.
type NoAssetsError struct {
	Repo Repo
	Tag  string
}

func (e *NoAssetsError) Error() string {
	return fmt.Sprintf("release %s of %s publishes no assets; build it yourself and use --binary", e.Tag, e.Repo)
}

// Descriptor fetches the repository's outpost.toml.
//
// The release's own copy is preferred over the repository's, because it is the
// one that shipped with this binary; a descriptor edited on the default branch
// since the release would otherwise describe a build nobody is installing.
// A repository with no descriptor is not an error — it just means `add` falls
// back to the defaults and whatever flags were passed.
func (c *Client) Descriptor(ctx context.Context, repo Repo, rel *Release) (*Descriptor, error) {
	for _, a := range rel.Assets {
		if strings.EqualFold(a.Name, DescriptorName) {
			var buf strings.Builder
			if _, err := c.downloadText(ctx, a.downloadURL(), &buf); err != nil {
				return nil, fmt.Errorf("fetch %s from release %s: %w", DescriptorName, rel.Tag, err)
			}
			return ParseDescriptor([]byte(buf.String()))
		}
	}

	// Fall back to the tagged tree, which is still the code that was released.
	// The tag is escaped: a git tag may hold "#", "?", "&" or a space, and
	// interpolating one raw would truncate the query and silently fetch the
	// default branch's descriptor — exactly the drift this fallback avoids.
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s",
		strings.TrimSuffix(c.BaseURL, "/"), repo.Owner, repo.Name, DescriptorName, neturl.QueryEscape(rel.Tag))
	resp, err := c.get(ctx, url, "application/vnd.github.raw")
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", DescriptorName, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, nil
	default:
		return nil, fmt.Errorf("fetch %s: GitHub returned %s", DescriptorName, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTextBytes))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", DescriptorName, err)
	}
	return ParseDescriptor(body)
}

// NoPlatformAssetError is a release with nothing built for this platform. For
// `add` the fix is usually the descriptor's asset pattern; for an update it is
// usually a build that failed for this platform alone.
type NoPlatformAssetError struct {
	Tag          string
	GOOS, GOARCH string
}

func (e *NoPlatformAssetError) Error() string {
	return fmt.Sprintf("release %s publishes no asset for %s/%s; "+
		"name one with `asset = \"...${GOOS}-${GOARCH}\"` in %s, or use --binary",
		e.Tag, e.GOOS, e.GOARCH, DescriptorName)
}

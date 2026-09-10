// Package acquire installs a service's binary from a GitHub release, so adding
// a service is sharing a repository URL rather than building it by hand first.
//
// Release assets only: Outpost never builds from source. Running a repository's
// build scripts to install it is a different and much larger trust decision
// than downloading a binary its author published, and one this package
// deliberately does not make. A repository with no usable release gets a clear
// error pointing at --binary.
package acquire

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// Repo is a GitHub owner and repository name.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

// segment is what GitHub allows in an owner or repository name.
var segment = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepo accepts the forms someone actually has to hand: a browser URL, an
// scp-style or https clone URL, or plain owner/repo.
func ParseRepo(s string) (Repo, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Repo{}, fmt.Errorf("empty repository reference")
	}

	// git@github.com:owner/repo.git
	if rest, ok := strings.CutPrefix(raw, "git@github.com:"); ok {
		return splitOwnerRepo(rest)
	}

	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return Repo{}, fmt.Errorf("parse %q: %w", s, err)
		}
		host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		if host != "github.com" {
			return Repo{}, fmt.Errorf("only github.com is supported, got %q", u.Host)
		}
		return splitOwnerRepo(u.Path)
	}

	// github.com/owner/repo, with no scheme.
	if rest, ok := strings.CutPrefix(raw, "github.com/"); ok {
		return splitOwnerRepo(rest)
	}
	return splitOwnerRepo(raw)
}

// splitOwnerRepo takes the path part and validates both halves. Anything
// deeper than owner/repo is accepted and truncated, so a URL copied from a
// file or releases page still works.
func splitOwnerRepo(path string) (Repo, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return Repo{}, fmt.Errorf("expected owner/repo, got %q", strings.Trim(path, "/"))
	}
	owner, name := parts[0], strings.TrimSuffix(parts[1], ".git")
	// "." and ".." satisfy the character class but are not names, and the
	// repository name goes on to become a service name and a file path.
	for _, part := range []string{owner, name} {
		if !segment.MatchString(part) || part == "." || part == ".." {
			return Repo{}, fmt.Errorf("invalid owner or repository name in %q", path)
		}
	}
	return Repo{Owner: owner, Name: name}, nil
}

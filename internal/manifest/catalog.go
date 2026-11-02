package manifest

import (
	"fmt"
	"net/netip"
	"path"
	"regexp"
	"strings"
	"unicode"
)

// Catalog fields are what the gateway shows about an app and who it lets in.
// They're all optional: a manifest without them behaves exactly as before.

const (
	maxTitle       = 60
	maxDescription = 160
	maxHostname    = 253
)

// accountName matches auth's rule for usernames and groups. It's repeated
// here rather than imported, so the manifest package stays free of the auth
// store and its SQLite driver.
var accountName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// GatewayAddress is the address the gateway serves the app at, relative to
// its domain: a full hostname when one is set, otherwise the subdomain, which
// defaults to the service name.
func (s *Service) GatewayAddress() string {
	if s.Hostname != "" {
		return s.Hostname
	}
	if s.Subdomain != "" {
		return s.Subdomain
	}
	return s.Name
}

func (s *Service) validateCatalog() []error {
	var errs []error
	if err := oneLine("title", s.Title, maxTitle); err != nil {
		errs = append(errs, err)
	}
	if err := oneLine("description", s.Description, maxDescription); err != nil {
		errs = append(errs, err)
	}
	if s.Subdomain != "" && !validLabel(s.Subdomain) {
		errs = append(errs, fmt.Errorf("subdomain %q must be one DNS label: 1-63 characters of a-z, 0-9 and '-', not starting or ending with '-'", s.Subdomain))
	}
	if s.Hostname != "" {
		if s.Subdomain != "" {
			errs = append(errs, fmt.Errorf("set subdomain or hostname, not both: hostname replaces the whole address"))
		}
		if err := validHostname(s.Hostname); err != nil {
			errs = append(errs, err)
		}
	}
	for _, a := range s.Allow {
		if err := validAllow(a); err != nil {
			errs = append(errs, err)
		}
	}
	for _, p := range s.PublicPaths {
		if err := ValidPublicPath(p); err != nil {
			errs = append(errs, err)
		}
	}
	if s.MCPPath != "" {
		if err := validMCPPath(s.MCPPath); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// validMCPPath checks mcp_path: one absolute path in its simplest form, which
// the gateway sets on the request as is. Nothing it would have to escape, and
// nothing under the gateway's own /.outpost/.
func validMCPPath(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("mcp_path %q must start with \"/\"", p)
	}
	if strings.ContainsAny(p, "?#") {
		return fmt.Errorf("mcp_path %q is a path, without a query or fragment", p)
	}
	if strings.ContainsAny(p, "%\\;") || strings.IndexFunc(p, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) >= 0 {
		return fmt.Errorf("mcp_path %q has characters that would need escaping; use a plain path like \"/mcp\"", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("mcp_path %q isn't in its simplest form; write %q", p, path.Clean(p))
	}
	if p == "/.outpost" || strings.HasPrefix(p, "/.outpost/") {
		return fmt.Errorf("mcp_path %q is under /.outpost/, which belongs to the gateway", p)
	}
	return nil
}

// oneLine checks a catalog string: printable, no line breaks, not too long.
func oneLine(field, v string, max int) error {
	if len([]rune(v)) > max {
		return fmt.Errorf("%s is %d characters; keep it to %d", field, len([]rune(v)), max)
	}
	for _, r := range v {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%s must be one line of printable text", field)
		}
	}
	return nil
}

// validAllow accepts a username or "group:<name>".
func validAllow(a string) error {
	name, isGroup := strings.CutPrefix(a, "group:")
	if accountName.MatchString(name) {
		return nil
	}
	kind := "a username"
	if isGroup {
		kind = "a group name"
	}
	return fmt.Errorf("allow entry %q isn't %s; use a username or group:<name>, each 1-32 characters of a-z, 0-9, '.', '_' or '-'", a, kind)
}

// ValidPublicPath accepts a path the gateway serves without a login: an
// exact path, or a prefix ending in "/*". Anything more general would make
// it hard to see at a glance what a manifest leaves open.
func ValidPublicPath(p string) error {
	prefix, wild := strings.CutSuffix(p, "*")
	switch {
	case !strings.HasPrefix(p, "/"):
		return fmt.Errorf("public path %q must start with /", p)
	case wild && !strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("public path %q: a wildcard may only follow a slash, as in /webhook/*", p)
	case strings.Contains(prefix, "*"):
		return fmt.Errorf("public path %q: only one trailing * is allowed", p)
	case p == "/*":
		return fmt.Errorf("public path %q would open the whole app; drop the gateway for this service instead", p)
	case strings.HasPrefix(p, "/.outpost/"):
		return fmt.Errorf("public path %q is inside /.outpost/, which the gateway keeps for itself", p)
	case strings.ContainsAny(p, "?#\\%") || strings.ContainsFunc(p, unicode.IsControl):
		return fmt.Errorf("public path %q must be a plain path, with no query, fragment, escapes or backslashes", p)
	}
	// Compare against the cleaned form, keeping a trailing slash, so a
	// manifest can't say /a/../b and mean /b.
	clean := path.Clean(prefix)
	if strings.HasSuffix(prefix, "/") && clean != "/" {
		clean += "/"
	}
	if clean != prefix {
		return fmt.Errorf("public path %q isn't in its simplest form; write %q", p, clean+map[bool]string{true: "*"}[wild])
	}
	return nil
}

// MatchPublicPath reports whether a request path is covered by one of the
// patterns. The request path must already be clean; the gateway refuses
// anything else before asking.
func MatchPublicPath(patterns []string, reqPath string) bool {
	for _, p := range patterns {
		if prefix, ok := strings.CutSuffix(p, "*"); ok {
			if strings.HasPrefix(reqPath, prefix) {
				return true
			}
		} else if reqPath == p {
			return true
		}
	}
	return false
}

func validLabel(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func validHostname(h string) error {
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("hostname %q is an IP address; the gateway routes by name", h)
	}
	if len(h) > maxHostname || !strings.Contains(h, ".") {
		return fmt.Errorf("hostname %q must be a full lowercase name, like yarr.tail1234.ts.net", h)
	}
	for _, label := range strings.Split(h, ".") {
		if !validLabel(label) {
			return fmt.Errorf("hostname %q must be a full lowercase name, like yarr.tail1234.ts.net", h)
		}
	}
	return nil
}

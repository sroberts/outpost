package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"unicode"
)

// DefaultGatewayListen is where the gateway listens when listen is unset:
// loopback, for something in front (tailscale serve, Caddy) to forward to.
const DefaultGatewayListen = "127.0.0.1:8443"

// Gateway settings. Nothing here matters unless Enabled is true.
type Gateway struct {
	// Enabled starts the gateway with the daemon.
	Enabled bool `toml:"enabled"`
	// Listen is the address the gateway binds.
	Listen string `toml:"listen"`
	// PublicURL is the portal's address as browsers see it. Its scheme
	// decides whether cookies are Secure.
	PublicURL string `toml:"public_url"`
	// Domain is the parent of app hosts, <name>.<domain>. Defaults to
	// PublicURL's host.
	Domain string `toml:"domain"`
	// TLSCert and TLSKey serve TLS directly. Leave both empty when
	// something in front terminates TLS.
	TLSCert string `toml:"tls_cert"`
	TLSKey  string `toml:"tls_key"`
	// TrustedProxies may set X-Forwarded-For, -Host and -Proto. Unset means
	// loopback only; an empty list trusts nobody.
	TrustedProxies []string `toml:"trusted_proxies"`
	// Name is what the portal calls itself, on the sign-in page and above
	// the apps. Defaults to "Outpost".
	Name string `toml:"name"`
	// AllowInsecure permits a plain-http public_url on a non-loopback
	// listen address, which sends passwords across the network in clear.
	AllowInsecure bool `toml:"allow_insecure"`
}

// ListenAddr is Listen, or the default.
func (g Gateway) ListenAddr() string {
	if g.Listen == "" {
		return DefaultGatewayListen
	}
	return g.Listen
}

// maxName bounds the portal's name; it heads a phone screen.
const maxName = 60

// PortalName is Name, or the default.
func (g Gateway) PortalName() string {
	if g.Name == "" {
		return "Outpost"
	}
	return g.Name
}

// AppDomain is the parent domain of app hosts.
func (g Gateway) AppDomain() string {
	if g.Domain != "" {
		return strings.ToLower(g.Domain)
	}
	if u, err := url.Parse(g.PublicURL); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return ""
}

// Proxies returns the trusted proxy ranges. Validate has already checked
// they parse; anything that doesn't is skipped.
func (g Gateway) Proxies() []netip.Prefix {
	list := g.TrustedProxies
	if list == nil {
		list = []string{"127.0.0.1/32", "::1/128"}
	}
	var out []netip.Prefix
	for _, s := range list {
		if p, err := parsePrefix(s); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// parsePrefix accepts a CIDR or a bare address.
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		return p.Masked(), err
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}

func (g Gateway) validate() error {
	if !g.Enabled {
		return nil
	}
	if g.PublicURL == "" {
		return errors.New("public_url is required when the gateway is enabled")
	}
	u, err := url.Parse(g.PublicURL)
	if err != nil {
		return fmt.Errorf("public_url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("public_url %q: scheme must be http or https", g.PublicURL)
	}
	if u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("public_url %q: want just a scheme and host, like https://outpost.example", g.PublicURL)
	}
	if err := validHostname(u.Hostname()); err != nil {
		return fmt.Errorf("public_url: %w", err)
	}
	if len([]rune(g.Name)) > maxName {
		return fmt.Errorf("name is %d characters; keep it to %d", len([]rune(g.Name)), maxName)
	}
	for _, r := range g.Name {
		if !unicode.IsPrint(r) {
			return errors.New("name must be one line of printable text")
		}
	}
	if g.Domain != "" {
		if err := validHostname(g.Domain); err != nil {
			return fmt.Errorf("domain: %w", err)
		}
	}
	if (g.TLSCert == "") != (g.TLSKey == "") {
		return errors.New("tls_cert and tls_key go together: set both or neither")
	}
	host, _, err := net.SplitHostPort(g.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen %q: %w", g.ListenAddr(), err)
	}
	for _, p := range g.TrustedProxies {
		if _, err := parsePrefix(p); err != nil {
			return fmt.Errorf("trusted_proxies: %q is not an address or CIDR", p)
		}
	}
	if u.Scheme == "http" && !isLoopbackHost(host) && !g.AllowInsecure {
		return fmt.Errorf("public_url is plain http and listen %q is reachable from the network, so passwords "+
			"would cross it in clear; use https, listen on loopback behind something that terminates TLS, "+
			"or set allow_insecure = true", g.ListenAddr())
	}
	return nil
}

// validHostname accepts a DNS name that can have subdomains: lowercase
// labels of a-z, 0-9 and '-', and not an IP address.
func validHostname(h string) error {
	if h == "" {
		return errors.New("empty host name")
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return fmt.Errorf("%q is an IP address; apps need a domain name to have subdomains of", h)
	}
	if len(h) > 253 {
		return fmt.Errorf("%q is too long for a host name", h)
	}
	for _, label := range strings.Split(h, ".") {
		if !ValidLabel(label) {
			return fmt.Errorf("%q is not a valid lowercase host name", h)
		}
	}
	return nil
}

// ValidLabel reports whether s is one DNS label: 1–63 characters of a-z, 0-9
// and '-', not starting or ending with '-'.
func ValidLabel(s string) bool {
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

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	a, err := netip.ParseAddr(h)
	return err == nil && a.IsLoopback()
}

// render writes the [gateway] section. Save rewrites the whole file from
// Render, so every set value must come back out.
func (g Gateway) render(b *strings.Builder) {
	b.WriteString("[gateway]\n")
	b.WriteString("# A login front door: sign in once at public_url, then reach each\n")
	b.WriteString("# service at <name>.<domain>, which is told who is calling.\n")
	fmt.Fprintf(b, "enabled = %t\n", g.Enabled)
	b.WriteString("# What the portal calls itself; defaults to Outpost.\n")
	optional := func(key, val, example string) {
		if val != "" {
			fmt.Fprintf(b, "%s = %q\n", key, val)
		} else {
			fmt.Fprintf(b, "# %s = %q\n", key, example)
		}
	}
	optional("name", g.Name, "Home Outpost")
	optional("listen", g.Listen, DefaultGatewayListen)
	optional("public_url", g.PublicURL, "https://outpost.example")
	b.WriteString("# Apps are served at <name>.<domain>; defaults to public_url's host.\n")
	optional("domain", g.Domain, "outpost.example")
	b.WriteString("# Serve TLS directly. Leave both unset when something in front terminates TLS.\n")
	optional("tls_cert", g.TLSCert, "/path/to/fullchain.pem")
	optional("tls_key", g.TLSKey, "/path/to/privkey.pem")
	b.WriteString("# Proxies allowed to set X-Forwarded-*. Unset means loopback only.\n")
	if g.TrustedProxies != nil {
		quoted := make([]string, len(g.TrustedProxies))
		for i, p := range g.TrustedProxies {
			quoted[i] = fmt.Sprintf("%q", p)
		}
		fmt.Fprintf(b, "trusted_proxies = [%s]\n", strings.Join(quoted, ", "))
	} else {
		b.WriteString("# trusted_proxies = [\"127.0.0.1/32\", \"::1/128\"]\n")
	}
	if g.AllowInsecure {
		b.WriteString("allow_insecure = true\n")
	}
}

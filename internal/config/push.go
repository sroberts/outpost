package config

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DefaultPushListen is where the push endpoint listens unless [push] says.
const DefaultPushListen = "127.0.0.1:47903"

// Push is how Outpost sends notifications to people's devices. It needs the
// login gateway, since a device subscribes on the portal. See
// docs/design/push.md.
type Push struct {
	// Listen is the loopback address of the endpoint apps call, which they
	// are told as OUTPOST_PUSH_URL.
	Listen string `toml:"listen"`
	// Contact is how a push service (Apple's, Google's, Mozilla's) can reach
	// the operator: a mailto: or https: URI. Unset is the portal's public
	// URL when that is https.
	Contact string `toml:"contact"`
}

// ListenAddr is Listen, or the default.
func (p Push) ListenAddr() string { return orDefault(p.Listen, DefaultPushListen) }

// ContactFor is Contact, or else the portal's address if that is https, or
// else "", meaning there's nothing a push service will take.
func (p Push) ContactFor(publicURL string) string {
	if p.Contact != "" {
		return p.Contact
	}
	if u, err := url.Parse(publicURL); err == nil && u.Scheme == "https" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return ""
}

func (p Push) validate() error {
	host, port, err := net.SplitHostPort(p.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen %q: want host:port, like %s", p.Listen, DefaultPushListen)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen %q: the push endpoint is for apps on this machine, so it has to be a loopback address, like %s", p.Listen, DefaultPushListen)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("listen %q: give a fixed port; apps keep the address across daemon restarts", p.Listen)
	}
	if p.Contact != "" {
		u, err := url.Parse(p.Contact)
		ok := err == nil && ((u.Scheme == "mailto" && strings.Contains(u.Opaque, "@")) || (u.Scheme == "https" && u.Host != ""))
		if !ok {
			return fmt.Errorf("contact %q: want a mailto: or https: address, like mailto:you@example.com; push services refuse anything else", p.Contact)
		}
	}
	return nil
}

// render writes [push] only when something in it is set, so a file that
// never mentioned it is written as it was.
func (p Push) render(b *strings.Builder) {
	if p == (Push{}) {
		return
	}
	b.WriteString("\n[push]\n")
	b.WriteString("# Notifications to people's devices. Every app gets OUTPOST_PUSH_URL\n")
	b.WriteString("# and its own OUTPOST_PUSH_TOKEN. Needs the gateway.\n")
	if p.Contact != "" {
		fmt.Fprintf(b, "contact = %q\n", p.Contact)
	}
	if p.Listen != "" {
		fmt.Fprintf(b, "listen = %q\n", p.Listen)
	}
}

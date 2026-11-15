package gateway

import "net/url"

// ClientLabel names an OAuth client for a person: the name its metadata
// document gives, which the client chose, beside the host that document is
// served from, which it can't choose. "Claude (claude.ai)", or the host alone
// when there is no name. See docs/design/mcp-oauth.md.
func ClientLabel(name, clientID string) string {
	host := clientID
	if u, err := url.Parse(clientID); err == nil && u.Host != "" {
		host = u.Host
	}
	if name == "" || name == host {
		return host
	}
	return name + " (" + host + ")"
}

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// An OAuth client names itself by the URL of its Client ID Metadata Document
// (draft-ietf-oauth-client-id-metadata-document), which the gateway fetches
// when someone is asked to let it in. The URL is chosen by whoever made the
// authorize link, so fetching it is the one place the gateway makes a request
// to an address someone else picked. Only https on 443 to a public address,
// checked on the address actually dialled, no redirects, no proxy, five
// seconds and five kilobytes.

const (
	clientMetaTimeout = 5 * time.Second
	clientMetaMaxBody = 5 << 10
	clientMetaTTL     = time.Hour
	clientMetaCached  = 100
	maxClientID       = 2048
)

// clientMeta is what the gateway uses from a client's metadata document.
type clientMeta struct {
	ClientID     string   `json:"client_id"`
	ClientName   string   `json:"client_name"`
	RedirectURIs []string `json:"redirect_uris"`
	// A client with a secret, or another way to authenticate at the token
	// endpoint, isn't one this server can serve: it has no secrets to check.
	TokenEndpointAuthMethod string `json:"token_endpoint_auth_method"`
	ClientSecret            string `json:"client_secret"`
}

// clientMetaCache keeps valid documents for clientMetaTTL, so the consent
// page and its form don't fetch twice. Errors and invalid documents are
// never kept (CIMD §6.2).
type clientMetaCache struct {
	mu sync.Mutex
	m  map[string]cachedClientMeta
}

type cachedClientMeta struct {
	meta    clientMeta
	expires time.Time
}

// checkClientID applies CIMD §3's rules to a client_id before anything is
// fetched: https, a host, a path other than "/", no fragment, no userinfo
// and no dot segments.
func checkClientID(id string) error {
	if id == "" || len(id) > maxClientID {
		return errors.New("the client didn't say who it is")
	}
	u, err := url.Parse(id)
	switch {
	case err != nil:
		return errors.New("the client's identity isn't a URL")
	case u.Scheme != "https" || u.Host == "" || u.Opaque != "":
		return errors.New("the client's identity isn't an https URL")
	case u.User != nil || u.Fragment != "" || strings.Contains(id, "#"):
		return errors.New("the client's identity URL has parts it mustn't")
	case u.Path == "" || u.Path == "/":
		return errors.New("the client's identity URL has no path")
	}
	for _, seg := range strings.Split(u.EscapedPath(), "/") {
		if seg == "." || seg == ".." || strings.EqualFold(seg, "%2e") || strings.EqualFold(seg, "%2e%2e") {
			return errors.New("the client's identity URL has dot segments")
		}
	}
	return nil
}

// clientMetadata returns a client's metadata document, from the cache or
// fetched. The document must name itself by the URL it came from.
func (g *Gateway) clientMetadata(ctx context.Context, id string) (clientMeta, error) {
	if err := checkClientID(id); err != nil {
		return clientMeta{}, err
	}
	now := g.now()
	g.clientMetas.mu.Lock()
	if c, ok := g.clientMetas.m[id]; ok && now.Before(c.expires) {
		g.clientMetas.mu.Unlock()
		return c.meta, nil
	}
	g.clientMetas.mu.Unlock()

	m, err := g.fetchClientMeta(ctx, id)
	if err != nil {
		g.lg("client", id, "err", err).Warnf("gateway: oauth: client %s: %v", id, err)
		return clientMeta{}, err
	}
	g.clientMetas.mu.Lock()
	if g.clientMetas.m == nil || len(g.clientMetas.m) >= clientMetaCached {
		g.clientMetas.m = map[string]cachedClientMeta{}
	}
	g.clientMetas.m[id] = cachedClientMeta{meta: m, expires: now.Add(clientMetaTTL)}
	g.clientMetas.mu.Unlock()
	return m, nil
}

func (g *Gateway) fetchClientMeta(ctx context.Context, id string) (clientMeta, error) {
	base := g.clientMetaHTTP
	if base == nil {
		base = publicOnlyClient()
	}
	// Never a redirect (CIMD §6), whichever client fetches: the document
	// has to be at the URL that names it.
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	ctx, cancel := context.WithTimeout(ctx, clientMetaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, id, nil)
	if err != nil {
		return clientMeta{}, errors.New("the client's identity URL can't be fetched")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return clientMeta{}, fmt.Errorf("couldn't fetch the client's details: %w", err)
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	if resp.StatusCode != http.StatusOK {
		return clientMeta{}, fmt.Errorf("the client's details answered %s, not 200", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, clientMetaMaxBody+1))
	if err != nil {
		return clientMeta{}, fmt.Errorf("couldn't read the client's details: %w", err)
	}
	if len(body) > clientMetaMaxBody {
		return clientMeta{}, errors.New("the client's details are larger than 5 KB")
	}
	var m clientMeta
	if err := json.Unmarshal(body, &m); err != nil {
		return clientMeta{}, errors.New("the client's details aren't valid JSON")
	}
	switch {
	case m.ClientID != id:
		return clientMeta{}, errors.New("the client's details name a different client")
	case len(m.RedirectURIs) == 0:
		return clientMeta{}, errors.New("the client's details list no redirect_uris")
	case m.ClientSecret != "" || (m.TokenEndpointAuthMethod != "" && m.TokenEndpointAuthMethod != "none"):
		return clientMeta{}, errors.New("the client authenticates with a secret or key, which this server doesn't take")
	}
	m.ClientName = auth.CleanClientName(m.ClientName)
	return m, nil
}

// publicOnlyClient fetches only from public addresses on port 443, follows
// no redirects and uses no proxy, so a client_id can't be used to reach
// anything on the machine, the LAN or the tailnet.
func publicOnlyClient() *http.Client {
	dialer := &net.Dialer{Timeout: clientMetaTimeout, Control: publicDialControl}
	return &http.Client{
		Timeout: clientMetaTimeout,
		Transport: &http.Transport{
			Proxy:                  nil,
			DialContext:            dialer.DialContext,
			TLSHandshakeTimeout:    clientMetaTimeout,
			ResponseHeaderTimeout:  clientMetaTimeout,
			MaxResponseHeaderBytes: 16 << 10,
			ForceAttemptHTTP2:      true,
		},
	}
}

// publicDialControl refuses a connection to anything but a public address on
// port 443. It runs on the address being dialled, after DNS, so a name that
// resolves somewhere private is refused too.
func publicDialControl(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("refusing to dial %s", address)
	}
	if ap.Port() != 443 {
		return fmt.Errorf("refusing to dial port %d: only 443", ap.Port())
	}
	if !publicAddr(ap.Addr()) {
		return fmt.Errorf("refusing to dial %s: not a public address", ap.Addr())
	}
	return nil
}

// nonPublic are special-use ranges the standard library's predicates don't
// cover. 100.64.0.0/10 is shared address space, and where Tailscale puts
// every node.
var nonPublic = func() []netip.Prefix {
	var out []netip.Prefix
	for _, s := range []string{
		"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15",
		"198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4",
		"64:ff9b::/96", "64:ff9b:1::/48", "2001::/32", "2001:db8::/32", "2002::/16",
	} {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}()

func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	for _, p := range nonPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

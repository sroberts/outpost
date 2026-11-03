package tsnet

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A tcp forward can't pass through the gateway's login: there is no request
// to send it, only a byte stream. So with the gateway on, a node asks it
// instead, once per connection, whether the caller's tailnet login belongs to
// an Outpost user who may open the service. These are the two ends of that
// question.
const (
	// AccessPath is where the gateway answers it, on loopback only.
	AccessPath = "/.outpost/tailnet-access"
	// AccessSecretHeader carries the secret that shows the question comes
	// from one of the daemon's own nodes rather than any process on the
	// machine, which could otherwise learn whose logins are linked.
	AccessSecretHeader = "Outpost-Node-Secret"
	// AccessSecretEnv carries that secret to the node, for the same reason
	// AuthKeyEnv carries the auth key: arguments are visible in ps.
	AccessSecretEnv = "OUTPOST_TSNET_ACCESS_SECRET"
)

// accessTimeout bounds one question. The gateway answers from auth.db on
// loopback, so anything near this means it is wedged, and the connection is
// refused rather than left hanging.
const accessTimeout = 5 * time.Second

// AccessDecision is the gateway's answer.
type AccessDecision struct {
	// User is the Outpost user the login belongs to, when allowed.
	User string `json:"user,omitempty"`
	// Reason says why not, when refused.
	Reason string `json:"reason,omitempty"`
}

// ErrAccessRefused is a definite no from the gateway, as opposed to a failure
// to ask.
var ErrAccessRefused = errors.New("refused")

// AccessChecker asks the gateway about one caller.
type AccessChecker struct {
	endpoint string
	secret   string
	client   *http.Client
}

// NewAccessChecker asks the gateway at upstream, the URL a node already
// proxies to, verifying an https one's certificate for serverName.
func NewAccessChecker(upstream, serverName, secret string) (*AccessChecker, error) {
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("gateway upstream %q is not a URL", upstream)
	}
	if secret == "" {
		return nil, errors.New("no access secret: the daemon starts every node with one when the gateway is on")
	}
	transport := &http.Transport{
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
	}
	if serverName != "" {
		transport.TLSClientConfig = &tls.Config{ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	return &AccessChecker{
		endpoint: strings.TrimSuffix(u.String(), "/") + AccessPath,
		secret:   secret,
		client:   &http.Client{Transport: transport, Timeout: accessTimeout},
	}, nil
}

// Check asks whether login may reach service. It returns the Outpost user on
// yes, an error wrapping ErrAccessRefused with the gateway's reason on no, and
// any other error when it couldn't get an answer, which the caller must also
// treat as no.
func (c *AccessChecker) Check(ctx context.Context, service, login string) (string, error) {
	form := url.Values{"service": {service}, "login": {login}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set(AccessSecretHeader, c.secret)
	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ask the gateway: %w", err)
	}
	defer resp.Body.Close()
	var d AccessDecision
	// A small answer; anything longer isn't one.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&d); err != nil && resp.StatusCode == http.StatusOK {
		return "", fmt.Errorf("gateway's answer: %w", err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
		if d.User == "" {
			return "", errors.New("gateway allowed the connection but named no user")
		}
		return d.User, nil
	case http.StatusForbidden:
		if d.Reason == "" {
			d.Reason = "not allowed"
		}
		return "", fmt.Errorf("%w: %s", ErrAccessRefused, d.Reason)
	default:
		return "", fmt.Errorf("gateway answered %s", resp.Status)
	}
}

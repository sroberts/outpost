package ipc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/gateway"
)

// TokenRequest is the argument to a token.* command.
type TokenRequest struct {
	Username string `json:"username,omitempty"`
	Name     string `json:"name,omitempty"`
	// Apps limits the token; empty means every app the user may reach.
	Apps []string `json:"apps,omitempty"`
	// ExpiresIn is a duration rather than a time so the daemon's clock decides;
	// zero means the token doesn't expire.
	ExpiresIn time.Duration `json:"expires_in,omitempty"`
	ID        int64         `json:"id,omitempty"`
}

// TokenInfo is an access token as the CLI sees one. It has no hash or secret
// fields, so listing tokens can't leak one.
type TokenInfo struct {
	ID         int64     `json:"id"`
	Username   string    `json:"username"`
	Name       string    `json:"name,omitempty"`
	Apps       []string  `json:"apps,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Expired    bool      `json:"expired,omitempty"`
}

func tokenInfo(t auth.AccessToken, now time.Time) TokenInfo {
	return TokenInfo{
		ID: t.ID, Username: t.Username, Name: t.Name, Apps: t.Apps,
		CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt,
		Expired: t.Expired(now),
	}
}

// dispatchToken serves the token.* commands. Like dispatchAuth it goes straight
// to the auth store; only the feed entries reach the supervisor, through its
// own queue.
func (s *Server) dispatchToken(ctx context.Context, req Request) Response {
	if s.auth == nil {
		return errorResponse(errNoAuth)
	}
	if req.AccessToken == nil {
		return errorResponse(fmt.Errorf("%s: no arguments", req.Command))
	}
	r := *req.AccessToken
	switch req.Command {
	case CmdTokenCreate:
		if r.Username == "" {
			return errorResponse(errors.New("token create: a username is required"))
		}
		secret, t, err := s.auth.CreateAccessToken(r.Username, r.Name, r.Apps, r.ExpiresIn)
		if err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventTokenCreate, fmt.Sprintf("access token %d created for %s", t.ID, t.Username))
		return Response{OK: true, Secret: secret, Tokens: []TokenInfo{tokenInfo(t, time.Now())},
			Message: s.unknownApps(ctx, t.Apps)}

	case CmdTokenList:
		list, err := s.auth.ListAccessTokens(r.Username)
		if err != nil {
			return errorResponse(err)
		}
		infos := make([]TokenInfo, 0, len(list))
		for _, t := range list {
			infos = append(infos, tokenInfo(t, time.Now()))
		}
		return Response{OK: true, Tokens: infos}

	case CmdTokenRevoke:
		t, err := s.auth.RevokeAccessToken(r.ID)
		if err != nil {
			return errorResponse(err)
		}
		s.feed(ctx, gateway.EventTokenRevoke, fmt.Sprintf("access token %d for %s revoked", t.ID, t.Username))
		return Response{OK: true, Message: fmt.Sprintf("revoked access token %d for %s", t.ID, t.Username)}
	}
	return errorResponse(fmt.Errorf("unknown command %q", req.Command))
}

// unknownApps warns about scope entries that match no service. A manifest may
// come after the token, as with allow, so it's a warning and not a refusal;
// but a mistyped name would otherwise only show up as every request being
// rejected. Failing to check is no reason to fail the command.
func (s *Server) unknownApps(ctx context.Context, apps []string) string {
	if len(apps) == 0 {
		return ""
	}
	services, err := s.sup.List(ctx)
	if err != nil {
		s.logf("outpost: list services: %v", err)
		return ""
	}
	known := make(map[string]bool, len(services))
	for _, svc := range services {
		known[svc.Name] = true
	}
	var unknown []string
	for _, app := range apps {
		if !known[app] {
			unknown = append(unknown, app)
		}
	}
	if len(unknown) == 0 {
		return ""
	}
	return "no service is called " + strings.Join(unknown, ", ") + " yet; the token won't reach anything by that name until one is"
}

// liveTokens counts a user's tokens ahead of something that ends them. Only
// the feed entry depends on it, so a failure is logged and doesn't stop the
// command.
func (s *Server) liveTokens(username string) int {
	n, err := s.auth.CountAccessTokens(username)
	if err != nil {
		s.logf("outpost: %v", err)
	}
	return n
}

// tokensRevoked records tokens ended along with something else.
func (s *Server) tokensRevoked(ctx context.Context, n int, username, why string) {
	if n > 0 {
		s.feed(ctx, gateway.EventTokenRevoke, fmt.Sprintf("%d access token(s) for %s revoked: %s", n, username, why))
	}
}

// feed adds a line to the gateway's activity feed. A failed write is logged,
// not returned: the command itself has already happened.
func (s *Server) feed(ctx context.Context, kind, message string) {
	if err := s.sup.RecordEvent(ctx, gateway.ActivityService, kind, message); err != nil {
		s.logf("outpost: record %s event: %v", kind, err)
	}
}

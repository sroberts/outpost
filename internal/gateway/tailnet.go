package gateway

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"

	"github.com/sroberts/outpost/internal/auth"
	outposttsnet "github.com/sroberts/outpost/internal/tsnet"
)

// serveTailnetAccess answers a tailnet node asking whether a connection to
// one of its service's tcp forwards may go ahead: whether the caller's
// tailnet login belongs to an enabled Outpost user who may open that app.
//
// It is the login for streams the proxy can't carry, so it applies the same
// rules an app request meets after sign-in: a known, enabled user, and the
// app's allow list. Only the daemon's own nodes may ask, from loopback with
// the secret it started them with; anyone else learns nothing, not even
// that the endpoint is here.
func (g *Gateway) serveTailnetAccess(w http.ResponseWriter, r *http.Request) {
	if g.nodeSecret == "" || !fromLoopback(r.RemoteAddr) ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get(outposttsnet.AccessSecretHeader)), []byte(g.nodeSecret)) != 1 {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	service, login := r.PostForm.Get("service"), auth.NormalizeTailnetLogin(r.PostForm.Get("login"))

	user, reason := g.tailnetAccess(service, login)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if reason != "" {
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(outposttsnet.AccessDecision{Reason: reason}) // the node treats a lost answer as no
		return
	}
	_ = json.NewEncoder(w).Encode(outposttsnet.AccessDecision{User: user}) // as above
}

// tailnetAccess decides, returning the user on yes and why not otherwise.
func (g *Gateway) tailnetAccess(service, login string) (user, reason string) {
	rt, ok := g.routes.get(service)
	if !ok {
		return "", "no such service"
	}
	if login == "" {
		return "", "the caller has no tailnet login"
	}
	u, err := g.auth.UserForTailnetLogin(login)
	switch {
	case errors.Is(err, auth.ErrNoSuchUser):
		g.activity.failed(EventDenied, fmt.Sprintf("for tailnet login %s to %s", login, rt.Name), false)
		return "", fmt.Sprintf("tailnet login %s is not an Outpost user's; link it with `outpost user tailnet <name> %s`", login, login)
	case errors.Is(err, auth.ErrAmbiguousTailnetLogin):
		g.activity.failed(EventDenied, fmt.Sprintf("for tailnet login %s to %s", login, rt.Name), false)
		return "", err.Error()
	case err != nil:
		g.logf("gateway: tailnet access for %s to %s: %v", login, rt.Name, err)
		return "", "the gateway could not look the login up"
	}
	if u.Disabled {
		g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", u.Username, rt.Name), false)
		return "", u.Username + " is disabled"
	}
	if !allowed(u, rt) {
		g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", u.Username, rt.Name), false)
		return "", u.Username + " is not on " + rt.Name + "'s allow list"
	}
	return u.Username, ""
}

// fromLoopback reports whether a connection's remote address is this
// machine's. The Host header can't say: the client chooses it.
func fromLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.Unmap().IsLoopback()
}

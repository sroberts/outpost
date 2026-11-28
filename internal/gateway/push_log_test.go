package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/logging"
)

// serve gives the gateway a Logger and no Logf, so the push handlers' lines
// have to go through lg(): a bare g.logf is nil there, and a refused endpoint
// would panic the handler instead of getting its 400. The line names the
// endpoint's host and never the rest of it, which is the device's token.
func TestPushSubscription_RefusedEndpointLogsHostOnly(t *testing.T) {
	h := newHarness(t)
	var logs lockedBuffer
	h.g.logf, h.g.logger = nil, logging.Component(logging.New(&logs, "debug", "logfmt"), "gateway", "debug")
	withPush(t, h)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)
	h.login(c, "alice", "", "")

	const refused = "https://push.example.net/device-token-secret"
	if resp := h.sendJSON(c, http.MethodPost, portal+pushSubPath, portal, "application/json", browserSub(t, refused)); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unlisted push service: %d", resp.StatusCode)
	}
	out := logs.String()
	if !strings.Contains(out, "push.example.net") {
		t.Errorf("the log doesn't name the refused host:\n%s", out)
	}
	if strings.Contains(out, "device-token-secret") {
		t.Errorf("the log carries the endpoint's token:\n%s", out)
	}
}

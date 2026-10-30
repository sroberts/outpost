package supervisor

import (
	"context"
	"testing"
)

// The gateway's events land in the same feed as the supervisor's, under the
// service name it gives.
func TestRecordEvent(t *testing.T) {
	h := newHarness(t)
	if err := h.sup.RecordEvent(context.Background(), "gateway", "sign-in", "alice signed in from 192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	events, err := h.sup.Events(context.Background(), "gateway", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != "sign-in" || events[0].Message != "alice signed in from 192.0.2.1" {
		t.Errorf("events = %+v", events)
	}
}

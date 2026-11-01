package gateway

import (
	"testing"
	"time"
)

// A failure long past is forgotten even with room left in the table: an address
// that mistypes a password now and then isn't working up to a minute's lockout.
func TestThrottle_OldFailuresDoNotCount(t *testing.T) {
	var th throttle
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 8 {
		now = now.Add(throttleForget + time.Minute)
		th.fail(now, "ip:a")
	}
	if d := th.wait(now, "ip:a"); d != throttleBase {
		t.Errorf("after a failure well apart from the last: wait %v; want %v", d, throttleBase)
	}
}

// Failures close together still add up, or there'd be no backing off at all.
func TestThrottle_RecentFailuresStillCount(t *testing.T) {
	var th throttle
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for range 3 {
		now = now.Add(throttleForget / 2)
		th.fail(now, "ip:a")
	}
	if d := th.wait(now, "ip:a"); d != 4*throttleBase {
		t.Errorf("after three failures close together: wait %v; want %v", d, 4*throttleBase)
	}
}

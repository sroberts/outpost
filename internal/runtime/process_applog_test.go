package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
)

// Each line a real process writes is stored with when it was written and on
// which stream, so stdout and stderr can be told apart afterwards.
func TestProcessRuntime_StampsLinesWithTimeAndStream(t *testing.T) {
	r := testRuntime(t)
	svc := testService(t)
	before := time.Now().Add(-time.Second)

	h, err := r.Start(context.Background(), svc)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitHealthy(t, r, h, 5*time.Second)
	if err := r.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	recs, err := applog.Read(r.logDir(), svc.Name, applog.Query{Lines: applog.MaxLines})
	if err != nil {
		t.Fatal(err)
	}
	streams := map[string]string{}
	for _, rec := range recs {
		if rec.Time.Before(before) || rec.Time.After(time.Now().Add(time.Second)) {
			t.Errorf("line %q stamped %v, outside the run", rec.Line, rec.Time)
		}
		switch {
		case strings.Contains(rec.Line, "fakeservice: listening on port"):
			streams["listening"] = rec.Stream
		case strings.Contains(rec.Line, "fakeservice: stderr stream open"):
			streams["stderr"] = rec.Stream
		}
	}
	if streams["listening"] != applog.Stdout || streams["stderr"] != applog.Stderr {
		t.Errorf("streams = %v; want listening on out and the stderr line on err", streams)
	}
}

package state

import (
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpen_CreatesSchemaAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "running"}); err != nil {
		t.Fatalf("UpsertService: %v", err)
	}
	s.Close()

	// Reopening an existing DB must not wipe or fail on the existing schema.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	rec, ok, err := s2.GetService("a")
	if err != nil || !ok {
		t.Fatalf("GetService after reopen: ok=%v err=%v", ok, err)
	}
	if rec.Status != "running" {
		t.Errorf("Status = %q; want %q", rec.Status, "running")
	}
}

func TestUpsertService_InsertsThenUpdates(t *testing.T) {
	s := openTest(t)

	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "starting"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now().Truncate(time.Second)
	if err := s.UpsertService(ServiceRecord{
		Name: "a", Runtime: "process", Status: "running",
		InstanceID: "4242", RestartCount: 2, LastStartedAt: started,
	}); err != nil {
		t.Fatal(err)
	}

	rec, ok, err := s.GetService("a")
	if err != nil || !ok {
		t.Fatalf("GetService: ok=%v err=%v", ok, err)
	}
	if rec.Status != "running" {
		t.Errorf("Status = %q; want running", rec.Status)
	}
	if rec.InstanceID != "4242" {
		t.Errorf("InstanceID = %q; want 4242", rec.InstanceID)
	}
	if rec.RestartCount != 2 {
		t.Errorf("RestartCount = %d; want 2", rec.RestartCount)
	}
	if !rec.LastStartedAt.Equal(started) {
		t.Errorf("LastStartedAt = %v; want %v", rec.LastStartedAt, started)
	}
	if rec.UpdatedAt.IsZero() {
		t.Error("UpdatedAt was not set")
	}

	all, err := s.ListServices()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Errorf("ListServices returned %d rows; want 1 (upsert must not duplicate)", len(all))
	}
}

// Desired state is how the CLI tells a running supervisor what it wants, and it
// must survive a status update from the supervisor.
func TestDesiredState_DefaultsToRunningAndPersists(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "running"}); err != nil {
		t.Fatal(err)
	}

	rec, _, _ := s.GetService("a")
	if rec.Desired != DesiredRunning {
		t.Errorf("default Desired = %q; want %q", rec.Desired, DesiredRunning)
	}

	if err := s.SetDesired("a", DesiredStopped); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	rec, _, _ = s.GetService("a")
	if rec.Desired != DesiredStopped {
		t.Errorf("Desired = %q after a status upsert; want it preserved as %q", rec.Desired, DesiredStopped)
	}
}

func TestSetDesired_CreatesRowForUnknownService(t *testing.T) {
	s := openTest(t)
	// The CLI may stop a service the supervisor has not recorded yet.
	if err := s.SetDesired("ghost", DesiredStopped); err != nil {
		t.Fatalf("SetDesired: %v", err)
	}
	rec, ok, err := s.GetService("ghost")
	if err != nil || !ok {
		t.Fatalf("GetService: ok=%v err=%v", ok, err)
	}
	if rec.Desired != DesiredStopped {
		t.Errorf("Desired = %q; want %q", rec.Desired, DesiredStopped)
	}
}

func TestRequestRestart_BumpsEpoch(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	before, _, _ := s.GetService("a")

	if err := s.RequestRestart("a"); err != nil {
		t.Fatal(err)
	}
	after, _, _ := s.GetService("a")
	if after.RestartEpoch <= before.RestartEpoch {
		t.Errorf("RestartEpoch = %d; want > %d", after.RestartEpoch, before.RestartEpoch)
	}
	// A restart request implies the service should be running.
	if after.Desired != DesiredRunning {
		t.Errorf("Desired = %q after restart request; want %q", after.Desired, DesiredRunning)
	}
}

func TestDeleteService_RemovesRowAndEvents(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvent("a", EventStarted, "up"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteService("a"); err != nil {
		t.Fatal(err)
	}

	if _, ok, _ := s.GetService("a"); ok {
		t.Error("service row survived DeleteService")
	}
	evs, err := s.Events("a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 0 {
		t.Errorf("got %d events after DeleteService; want 0", len(evs))
	}
}

func TestGetService_MissingIsNotAnError(t *testing.T) {
	s := openTest(t)
	rec, ok, err := s.GetService("nope")
	if err != nil {
		t.Fatalf("GetService on a missing row returned an error: %v", err)
	}
	if ok {
		t.Errorf("ok = true for a missing row (got %+v)", rec)
	}
}

func TestEvents_NewestFirstAndScoped(t *testing.T) {
	s := openTest(t)
	for _, e := range []struct{ svc, kind, msg string }{
		{"a", EventStarted, "first"},
		{"b", EventStarted, "other service"},
		{"a", EventHealthFail, "second"},
		{"a", EventRestarted, "third"},
	} {
		if err := s.AppendEvent(e.svc, e.kind, e.msg); err != nil {
			t.Fatal(err)
		}
	}

	evs, err := s.Events("a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("Events(\"a\") returned %d; want 3 (scoped to one service)", len(evs))
	}
	if evs[0].Message != "third" {
		t.Errorf("first event = %q; want %q (newest first)", evs[0].Message, "third")
	}

	all, err := s.Events("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("Events(\"\") returned %d; want 4 (all services)", len(all))
	}

	limited, err := s.Events("a", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Errorf("Events with limit 2 returned %d", len(limited))
	}
}

func TestPruneEvents_ByAgeAndCount(t *testing.T) {
	s := openTest(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.appendEventAt("a", EventStarted, "ancient", old); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := s.AppendEvent("a", EventStarted, "recent"); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.PruneEvents(7*24*time.Hour, 1000); err != nil {
		t.Fatal(err)
	}
	evs, _ := s.Events("a", 100)
	if len(evs) != 5 {
		t.Errorf("after age prune: %d events; want 5", len(evs))
	}

	if err := s.PruneEvents(7*24*time.Hour, 2); err != nil {
		t.Fatal(err)
	}
	evs, _ = s.Events("a", 100)
	if len(evs) != 2 {
		t.Errorf("after row-cap prune: %d events; want 2", len(evs))
	}
}

func TestHalted_SetAndClearedByExplicitStart(t *testing.T) {
	s := openTest(t)
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "failed"}); err != nil {
		t.Fatal(err)
	}

	if err := s.SetHalted("a", true); err != nil {
		t.Fatal(err)
	}
	rec, _, _ := s.GetService("a")
	if !rec.Halted {
		t.Fatal("Halted = false after SetHalted(true)")
	}

	// A status update from the supervisor must not clear the halt.
	if err := s.UpsertService(ServiceRecord{Name: "a", Runtime: "process", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.GetService("a"); !rec.Halted {
		t.Error("Halted cleared by a routine status upsert")
	}

	// Asking for the service to run does clear it.
	if err := s.SetDesired("a", DesiredRunning); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.GetService("a"); rec.Halted {
		t.Error("Halted survived an explicit start")
	}
}

func TestHalted_ClearedByRestartRequest(t *testing.T) {
	s := openTest(t)
	if err := s.SetHalted("a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestRestart("a"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.GetService("a"); rec.Halted {
		t.Error("Halted survived a restart request")
	}
}

// Stopping a halted service must not silently un-halt it.
func TestHalted_NotClearedByStop(t *testing.T) {
	s := openTest(t)
	if err := s.SetHalted("a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDesired("a", DesiredStopped); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.GetService("a"); !rec.Halted {
		t.Error("Halted cleared by a stop")
	}
}

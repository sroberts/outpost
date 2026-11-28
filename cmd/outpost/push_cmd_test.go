package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/push"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeHalts is a supervisor whose list a test sets, signalling a change each
// time.
type fakeHalts struct {
	mu   sync.Mutex
	list []supervisor.ServiceStatus
	ch   chan struct{}
}

func (f *fakeHalts) Changed() <-chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ch
}

func (f *fakeHalts) List(context.Context) ([]supervisor.ServiceStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]supervisor.ServiceStatus(nil), f.list...), nil
}

func (f *fakeHalts) set(list ...supervisor.ServiceStatus) {
	f.mu.Lock()
	f.list = list
	close(f.ch)
	f.ch = make(chan struct{})
	f.mu.Unlock()
}

func TestWatchHalts_TellsAdminsOnceWhenAnAppStops(t *testing.T) {
	old := haltSettle
	haltSettle = 20 * time.Millisecond
	t.Cleanup(func() { haltSettle = old })
	src := &fakeHalts{ch: make(chan struct{}), list: []supervisor.ServiceStatus{
		{Name: "old", Halted: true}, // halted before this daemon: not news
		{Name: "yarr"},
	}}
	sent := make(chan push.Notification, 10)
	var to []string
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchHalts(ctx, src, func() []string { return []string{"root"} }, func(who []string, n push.Notification) (int, error) {
			to = who
			sent <- n
			return 1, nil
		}, t.Logf)
	}()
	defer func() { cancel(); <-done }()

	expect := func(title string) {
		t.Helper()
		select {
		case n := <-sent:
			if n.Title != title || n.Urgency != "high" || n.Path != "/" {
				t.Errorf("notification %+v, want %q", n, title)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no notification for %q", title)
		}
	}
	quiet := func() {
		t.Helper()
		select {
		case n := <-sent:
			t.Errorf("unexpected %+v", n)
		case <-time.After(haltSettle + 200*time.Millisecond):
		}
	}

	quiet()
	src.set(supervisor.ServiceStatus{Name: "old", Halted: true},
		supervisor.ServiceStatus{Name: "yarr", Title: "Yarr", Halted: true, RestartCount: 5})
	expect("Yarr has stopped")
	if len(to) != 1 || to[0] != "root" {
		t.Errorf("sent to %v", to)
	}
	// Still halted: not told again.
	src.set(supervisor.ServiceStatus{Name: "old", Halted: true},
		supervisor.ServiceStatus{Name: "yarr", Title: "Yarr", Halted: true})
	quiet()
	// Back up, then halted again: that's news again.
	src.set(supervisor.ServiceStatus{Name: "yarr", Title: "Yarr"})
	time.Sleep(haltSettle + 200*time.Millisecond)
	src.set(supervisor.ServiceStatus{Name: "yarr", Title: "Yarr", Halted: true})
	expect("Yarr has stopped")
}

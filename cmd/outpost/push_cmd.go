package main

import (
	"context"
	"fmt"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/push"
	"github.com/sroberts/outpost/internal/supervisor"
)

// haltSource is the supervisor as the halt watcher reads it: through its
// public API, like any other client.
type haltSource interface {
	Changed() <-chan struct{}
	List(ctx context.Context) ([]supervisor.ServiceStatus, error)
}

// haltSettle is how long the watcher lets changes gather before looking: a
// tick can signal many, and one look covers them all. A variable so tests
// needn't wait it out.
var haltSettle = time.Second

// watchHalts tells the admins, on their devices, when Outpost gives up
// restarting an app: the one thing that won't put itself right, and that an
// admin otherwise learns of only on opening the portal. An app already
// halted when the watcher starts was news to an earlier daemon, so it isn't
// told again.
func watchHalts(ctx context.Context, src haltSource, admins func() []string,
	send func(to []string, n push.Notification) (int, error), logf func(string, ...any)) {
	halted := map[string]bool{}
	first := true
	for {
		// Taken before listing, so a change during the list wakes the next.
		changed := src.Changed()
		lctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		list, err := src.List(lctx)
		cancel()
		if err == nil {
			now := map[string]bool{}
			for _, s := range list {
				if !s.Halted {
					continue
				}
				now[s.Name] = true
				if first || halted[s.Name] {
					continue
				}
				title := s.Title
				if title == "" {
					title = s.Name
				}
				n := push.Notification{
					Title:   title + " has stopped",
					Body:    fmt.Sprintf("Outpost gave up restarting it after %d tries. Open Outpost to see why.", s.RestartCount),
					Path:    "/",
					Tag:     "halt-" + s.Name,
					Urgency: "high",
				}
				if to := admins(); len(to) > 0 {
					if _, err := send(to, n); err != nil {
						logf("push: tell the admins %s stopped: %v", s.Name, err)
					}
				}
			}
			halted, first = now, false
		} else if ctx.Err() == nil {
			logf("push: list services: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-changed:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(haltSettle):
		}
	}
}

// enabledAdmins lists who Outpost's own notifications are for.
func enabledAdmins(store *auth.Store, logf func(string, ...any)) func() []string {
	return func() []string {
		users, err := store.ListUsers()
		if err != nil {
			logf("push: list admins: %v", err)
			return nil
		}
		var out []string
		for _, u := range users {
			if u.Admin && !u.Disabled {
				out = append(out, u.Username)
			}
		}
		return out
	}
}

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// updateAllCheckTimeout bounds asking about one service before updating any.
const updateAllCheckTimeout = 20 * time.Second

// updateAll runs `outpost update` for every service with a newer release, one
// at a time, and stops at the first that fails: a release that had to be rolled
// back is a reason for the operator to look before anything else changes.
//
// It asks GitHub itself rather than the daemon, since what gets installed
// shouldn't rest on an answer up to a day old, and because an update works with
// no daemon running.
func (a *app) updateAll(noBackup bool, wait time.Duration) error {
	services, err := manifest.LoadDir(a.servicesDir())
	if err != nil {
		return err
	}

	type pending struct {
		svc    *manifest.Service
		latest string
	}
	var queue []pending
	var unchecked, notes, noSource []string
	current := 0
	checks := &releaseChecks{a: a}
	for _, svc := range services {
		if svc.Source == "" {
			noSource = append(noSource, svc.Name)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), updateAllCheckTimeout)
		rc, err := checks.check(ctx, svc)
		cancel()
		switch {
		case err != nil:
			// This doesn't stop the others: nothing has been tried yet, and it
			// says nothing about whether their releases are any good.
			unchecked = append(unchecked, fmt.Sprintf("%s (%v)", svc.Name, err))
		case rc.Newer:
			queue = append(queue, pending{svc, rc.Latest})
		case rc.Note != "":
			notes = append(notes, svc.Name+": "+rc.Note)
		default:
			current++
		}
	}

	var flags []string
	if noBackup {
		flags = append(flags, "--no-backup")
	}
	if wait > 0 {
		flags = append(flags, "--wait", wait.String())
	}

	var updated, notTried []string
	var failed string
	var failure error
	for i, p := range queue {
		name, was := p.svc.Name, orDash(p.svc.Release)
		fmt.Fprintf(a.out, "updating %s: %s to %s\n", name, was, p.latest)
		// "--" because a manifest's name is free text, and this one mustn't be
		// read as a flag.
		if err := a.cmdUpdate(append(append([]string{}, flags...), "--", name)); err != nil {
			failed, failure = fmt.Sprintf("%s (%s to %s)", name, was, p.latest), fmt.Errorf("%s: %w", name, err)
			for _, rest := range queue[i+1:] {
				notTried = append(notTried, rest.svc.Name)
			}
			break
		}
		fmt.Fprintln(a.out)
		// A release published or installed since the check changes what this
		// did, so it's the manifest that says what's there now.
		now := p.latest
		if after, err := manifest.Load(filepath.Join(a.servicesDir(), name+".toml")); err == nil {
			now = after.Release
		}
		if sameRelease(p.svc.Release, now) {
			current++
			continue
		}
		updated = append(updated, fmt.Sprintf("%s (%s to %s)", name, was, now))
	}

	if len(updated) == 0 && failure == nil && len(unchecked) == 0 {
		fmt.Fprintf(a.out, "Nothing to update: %d up to date.\n", current)
	}
	report := func(label string, items []string, sep string) {
		if len(items) > 0 {
			fmt.Fprintf(a.out, "%s: %s\n", label, strings.Join(items, sep))
		}
	}
	report("Updated", updated, ", ")
	if failure != nil {
		fmt.Fprintf(a.out, "Failed: %s\n", failed)
	}
	report("Not tried", notTried, ", ")
	report("Not tried, the check failed", unchecked, "; ")
	report("Skipped", notes, "; ")
	report("No source to update from", noSource, ", ")

	switch {
	case failure != nil:
		return fmt.Errorf("update --all: %w", failure)
	case len(unchecked) > 0:
		return fmt.Errorf("update --all: couldn't check %d service(s)", len(unchecked))
	}
	return nil
}

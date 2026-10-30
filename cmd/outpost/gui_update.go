package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/manifest"
)

// daemonUpdates lets the gateway do what `outpost update` does, by running it.
// The daemon talks to itself over its own socket, exactly as the CLI would, so
// the backup, the lock, the health wait and the rollback are all the same code
// and nothing here reaches round the reconcile loop.
type daemonUpdates struct{ a *app }

// Update installs the latest release. Nothing else can be asked for: which
// release and whether to force it stay with the CLI, so the portal can't take
// a service backwards or over a release Update would otherwise refuse.
func (u daemonUpdates) Update(service string) (string, error) {
	var out bytes.Buffer
	run := *u.a
	run.out, run.err = &out, &out
	err := run.cmdUpdate([]string{service})
	return out.String(), err
}

// Latest says the same as `outpost update --check`, for one service.
func (u daemonUpdates) Latest(ctx context.Context, service string) (gateway.ReleaseCheck, error) {
	svc, err := manifest.Load(filepath.Join(u.a.servicesDir(), service+".toml"))
	if err != nil {
		return gateway.ReleaseCheck{}, err
	}
	repo, err := acquire.ParseRepo(svc.Source)
	if err != nil {
		return gateway.ReleaseCheck{}, fmt.Errorf("source %q: %w", svc.Source, err)
	}
	rel, err := acquireClient().LatestRelease(ctx, repo)
	var none *acquire.NoAssetsError
	if errors.As(err, &none) {
		return gateway.ReleaseCheck{
			Latest: none.Tag,
			Note:   none.Tag + " has nothing attached yet; its build may still be uploading.",
		}, nil
	}
	if err != nil {
		return gateway.ReleaseCheck{}, err
	}
	check := gateway.ReleaseCheck{Latest: rel.Tag}
	if sameRelease(svc.Release, rel.Tag) {
		return check, nil
	}
	if c, ok := compareReleases(svc.Release, rel.Tag); ok && c > 0 {
		check.Note = fmt.Sprintf("Newer than the latest release, %s. Going back takes `outpost update %s --to %s`.", rel.Tag, service, rel.Tag)
		return check, nil
	}
	// Checked here as well as by the update, so the page doesn't offer one
	// that's bound to be refused.
	if !u.a.ownsBinary(svc.Binary) {
		check.Note = fmt.Sprintf("%s is available, but %s isn't under Outpost's bin directory, so it isn't updated from here.", rel.Tag, svc.Binary)
		return check, nil
	}
	check.Newer = true
	return check, nil
}

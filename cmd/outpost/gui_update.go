package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/supervisor"
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
	return (&releaseChecks{a: u.a}).check(ctx, svc)
}

// supervisedUpdates is daemonUpdates with its checks made through the
// supervisor, so a check the page asks for updates what `outpost list` shows
// too, and there is one answer rather than two.
type supervisedUpdates struct {
	daemonUpdates
	sup *supervisor.Supervisor
}

func (u supervisedUpdates) Latest(ctx context.Context, service string) (gateway.ReleaseCheck, error) {
	return u.sup.CheckRelease(ctx, service)
}

// releaseChecks asks GitHub about services' latest releases. It remembers each
// repository's etag, so that asking again usually costs a token's rate limit
// nothing, along with what that etag answered.
type releaseChecks struct {
	a    *app
	mu   sync.Mutex
	seen map[string]seenRelease
}

// seenRelease is one repository's latest release as last reported.
type seenRelease struct {
	etag     string
	tag      string
	noAssets bool
}

// check works out where svc stands. Only the latest release comes from the
// repository, and the rest is worked out afresh each time, because an etag says
// nothing about what has been installed since.
func (r *releaseChecks) check(ctx context.Context, svc *manifest.Service) (supervisor.ReleaseCheck, error) {
	repo, err := acquire.ParseRepo(svc.Source)
	if err != nil {
		return supervisor.ReleaseCheck{}, fmt.Errorf("source %q: %w", svc.Source, err)
	}
	r.mu.Lock()
	seen := r.seen[repo.String()]
	r.mu.Unlock()

	client := acquireClient()
	// Each client has its own transport, and an idle connection left behind on
	// one would stay for as long as the daemon runs.
	defer client.HTTP.CloseIdleConnections()
	rel, etag, err := client.LatestReleaseSince(ctx, repo, seen.etag)
	var none *acquire.NoAssetsError
	switch {
	case errors.Is(err, acquire.ErrNotModified):
		// Nothing to store, and writing the copy read above back could undo
		// a newer answer another check stored meanwhile.
		return r.a.releaseVerdict(svc, seen), nil
	case errors.As(err, &none):
		seen = seenRelease{etag: etag, tag: none.Tag, noAssets: true}
	case err != nil:
		return supervisor.ReleaseCheck{}, err
	default:
		seen = seenRelease{etag: etag, tag: rel.Tag}
	}
	r.mu.Lock()
	if r.seen == nil {
		r.seen = map[string]seenRelease{}
	}
	r.seen[repo.String()] = seen
	r.mu.Unlock()
	return r.a.releaseVerdict(svc, seen), nil
}

// releaseVerdict is what the latest release means for one service.
func (a *app) releaseVerdict(svc *manifest.Service, latest seenRelease) supervisor.ReleaseCheck {
	check := supervisor.ReleaseCheck{Latest: latest.tag}
	if latest.noAssets {
		check.Note = latest.tag + " has nothing attached yet; its build may still be uploading."
		check.Pending = true
		return check
	}
	if sameRelease(svc.Release, latest.tag) {
		return check
	}
	if c, ok := compareReleases(svc.Release, latest.tag); ok && c > 0 {
		check.Note = fmt.Sprintf("Newer than the latest release, %s. Going back takes `outpost update %s --to %s`.", latest.tag, svc.Name, latest.tag)
		return check
	}
	// Checked here as well as by the update, so nothing offers one that's
	// bound to be refused.
	if !a.ownsBinary(svc.Binary) {
		check.Note = fmt.Sprintf("%s is available, but %s isn't under Outpost's bin directory, so it isn't updated from here.", latest.tag, svc.Binary)
		return check
	}
	check.Newer = true
	return check
}

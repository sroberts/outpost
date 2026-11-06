package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/sroberts/outpost/internal/exposure"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/supervisor"
)

// exposureWeb is the Internet access page's way to set an app's public paths
// and Funnel. The setting is held to the manifest's own rules, stored, and
// applied before it returns, so the page can say what went wrong.
type exposureWeb struct {
	a     *app
	store *exposure.Store
	sup   *supervisor.Supervisor
	opts  manifest.Options
}

func (e exposureWeb) Set(ctx context.Context, service string, funnel bool, publicPaths []string, by string) error {
	svc, err := e.manifest(service)
	if err != nil {
		return err
	}
	if _, errs := svc.WithExposure(funnel, publicPaths, e.opts); len(errs) > 0 {
		parts := make([]string, len(errs))
		for i, err := range errs {
			parts[i] = err.Error()
		}
		return errors.New(strings.Join(parts, "; "))
	}
	if err := e.store.Set(service, funnel, publicPaths, by); err != nil {
		return err
	}
	return e.apply(ctx)
}

func (e exposureWeb) Reset(ctx context.Context, service, _ string) error {
	if err := e.store.Reset(service); err != nil {
		return err
	}
	return e.apply(ctx)
}

// manifest is service's manifest as it is on disk, for checking a setting
// against.
func (e exposureWeb) manifest(service string) (*manifest.Service, error) {
	services, err := manifest.LoadDir(e.a.servicesDir())
	if err != nil {
		return nil, fmt.Errorf("read the manifests: %w", err)
	}
	for _, s := range services {
		if s.Name == service {
			return s, nil
		}
	}
	return nil, fmt.Errorf("%s has no manifest any more", service)
}

func (e exposureWeb) apply(ctx context.Context) error {
	if _, err := e.sup.ApplyNow(ctx); err != nil {
		return fmt.Errorf("saved, but the manifests didn't apply, so it takes effect once they do: %w", err)
	}
	return nil
}

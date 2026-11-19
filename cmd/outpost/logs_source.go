package main

import (
	"context"
	"errors"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/gateway"
)

// logsSource is the gateway's gateway.LogSource: the same files `outpost
// logs` reads, read in the daemon, which can see them.
type logsSource struct {
	dir string
	// services is the supervisor's List, for reading every service.
	services gateway.Services
}

func (l logsSource) Logs(ctx context.Context, service string, q applog.Query) ([]applog.Record, error) {
	if service != "" {
		recs, err := applog.Read(l.dir, service, q)
		if errors.Is(err, applog.ErrNoLog) {
			// Declared but silent so far, or never run: an empty log, not a
			// failure.
			return []applog.Record{}, nil
		}
		return recs, err
	}
	list, err := l.services.List(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list))
	for _, st := range list {
		names = append(names, st.Name)
	}
	return applog.ReadAll(l.dir, names, q)
}

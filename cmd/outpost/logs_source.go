package main

import (
	"context"
	"errors"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/supervisor"
)

// logsSource is the gateway's gateway.LogSource: the same files `outpost
// logs` reads, read in the daemon, which can see them.
type logsSource struct {
	dir string
	// services is the supervisor's List, for reading every service.
	services gateway.Services
	// events is the supervisor's activity feed, when there is one to read.
	events func(ctx context.Context, service string, limit int) ([]supervisor.EventRecord, error)
}

// The portal reads Outpost's own logs through it too.
var _ gateway.LogSources = logsSource{}

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

func (l logsSource) Source(ctx context.Context, source, service string, q applog.Query) ([]applog.Record, error) {
	return l.reader().Read(ctx, source, service, q)
}

func (l logsSource) reader() applog.Reader {
	rd := applog.Reader{Dir: l.dir, Services: func(ctx context.Context) ([]string, error) {
		list, err := l.services.List(ctx)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(list))
		for _, st := range list {
			names = append(names, st.Name)
		}
		return names, nil
	}}
	if l.events != nil {
		rd.Events = func(ctx context.Context, service string, limit int) ([]applog.Record, error) {
			evs, err := l.events(ctx, service, limit)
			return eventRecords(evs), err
		}
	}
	return rd
}

// eventRecords is the activity feed as log records, newest first as it came.
func eventRecords(evs []supervisor.EventRecord) []applog.Record {
	out := make([]applog.Record, len(evs))
	for i, e := range evs {
		out[i] = applog.FromEvent(e.Service, e.Kind, e.Message, e.At)
	}
	return out
}

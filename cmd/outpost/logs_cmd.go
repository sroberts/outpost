package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/manifest"
)

// cmdLogs shows or follows services' captured output, or with --source one
// of Outpost's own logs.
//
// Unlike the control commands this reads the log files directly: they are
// local, and reading them needs no daemon, so `outpost logs` still works
// when the daemon is down, which is often when it's wanted. The portal, its
// API and its MCP server read the same files through the daemon. The
// activity feed is the exception: it lives in the state store, and comes
// from the daemon.
func (a *app) cmdLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(a.err)
	follow := fs.Bool("f", false, "follow: keep printing lines as they're written")
	events := fs.Bool("events", false, "show the activity feed instead of the service's output")
	limit := fs.Int("n", 0, "how many lines to show (default 200; 20 with --events)")
	all := fs.Bool("all", false, "every service's output, interleaved by time")
	level := fs.String("level", "", "only lines at this level or above: debug, info, warn or error")
	since := fs.String("since", "", "only lines written since then: a length of time such as 10m or 2h, or a time such as 2026-11-18T09:00")
	grep := fs.String("grep", "", "only lines containing this text (ignoring case)")
	stream := fs.String("stream", "", "only lines from one stream: out or err")
	asJSON := fs.Bool("json", false, "print each line as a JSON object")
	raw := fs.Bool("raw", false, "print each line exactly as the service wrote it")
	source := fs.String("source", "", "which log: app (services' output, the default), daemon (Outpost's own), tailnet (its tailnet nodes'), access (every request the gateway answered), events (the activity feed) or all of them; a service name keeps the lines about it")
	until := fs.String("until", "", "only lines written before then, in the same forms as --since")
	user := fs.String("user", "", "only lines for this user, or for the access token with this id")
	status := fs.String("status", "", "only requests answered with this status, such as 404, or class, such as 5xx")
	names, err := parseOptionalService(fs, "logs", args)
	if err != nil {
		return err
	}
	if !applog.ValidSource(*source) {
		return fmt.Errorf("logs: --source %q: use one of %s", *source, strings.Join(applog.Sources, ", "))
	}
	own := *source != "" && *source != applog.SourceApp
	switch {
	case len(names) > 1:
		return fmt.Errorf("logs: unexpected argument %q", names[1])
	case own && *events:
		return errors.New("logs: --events is --source events; pick one")
	case own && *all:
		return errors.New("logs: --all is for services' output; --source reads every service's lines already")
	case own:
	case len(names) == 0 && !*all:
		return errors.New("logs: a service name is required, or --all for every service")
	case len(names) == 1 && *all:
		return errors.New("logs: name a service or pass --all, not both")
	}
	name := ""
	if len(names) == 1 {
		name = names[0]
	}

	// The activity feed lives in the state store and comes from the daemon,
	// unlike the log files, which are read directly (§8).
	if *events {
		if *all {
			return errors.New("logs: --events takes a service name")
		}
		n := *limit
		if n == 0 {
			n = 20
		}
		return a.showEvents(name, n)
	}

	q := applog.Query{Lines: *limit, Level: *level, Contains: *grep, Stream: *stream}
	if q.Lines == 0 {
		q.Lines = 200
	}
	if q.Level != "" && applog.NormalizeLevel(q.Level) == applog.LevelNone {
		return fmt.Errorf("logs: --level %q: use debug, info, warn or error", q.Level)
	}
	if q.Stream != "" && q.Stream != applog.Stdout && q.Stream != applog.Stderr {
		return fmt.Errorf("logs: --stream %q: use out or err", q.Stream)
	}
	if *since != "" {
		if q.Since, err = parseSince(*since, time.Now()); err != nil {
			return fmt.Errorf("logs: --since: %w", err)
		}
	}
	if *asJSON && *raw {
		return errors.New("logs: --json and --raw each choose the output; pick one")
	}
	q.User, q.Status = *user, *status
	if q.Status != "" && !applog.ValidStatus(q.Status) {
		return fmt.Errorf("logs: --status %q: use a code such as 404, or a class such as 5xx", q.Status)
	}
	if *until != "" {
		if q.Until, err = parseSince(*until, time.Now()); err != nil {
			return fmt.Errorf("logs: --until: %w", err)
		}
	}
	if own {
		p := logPrinter{out: a.out, json: *asJSON, raw: *raw, all: true, style: newLogStyles(a.out)}
		return a.ownLogs(*source, name, q, *follow, p)
	}

	read := func(q applog.Query) ([]applog.Record, error) {
		if !*all {
			return applog.Read(a.logsDir(), name, q)
		}
		services, err := manifest.LoadDir(a.servicesDir())
		if err != nil {
			return nil, err
		}
		var names []string
		for _, s := range services {
			names = append(names, s.Name)
		}
		sort.Strings(names)
		return applog.ReadAll(a.logsDir(), names, q)
	}
	p := logPrinter{out: a.out, json: *asJSON, raw: *raw, all: *all, style: newLogStyles(a.out)}

	recs, err := read(q)
	if errors.Is(err, applog.ErrNoLog) {
		path, _ := applog.Path(a.logsDir(), name)
		return fmt.Errorf("no logs for %s yet (expected %s)", name, path)
	}
	if err != nil {
		return err
	}
	for _, r := range recs {
		p.print(r)
	}
	if !*follow {
		return nil
	}

	// Following asks for what's newer than the last line shown. A line
	// stored in the same microsecond as it would be missed, which nothing
	// writing at human speed does.
	last := time.Now()
	if n := len(recs); n > 0 && !recs[n-1].Time.IsZero() {
		last = recs[n-1].Time
	}
	for {
		time.Sleep(300 * time.Millisecond)
		fq := q
		fq.Since, fq.Lines = last, applog.MaxLines
		more, err := read(fq)
		if err != nil && !errors.Is(err, applog.ErrNoLog) {
			return err
		}
		for _, r := range more {
			p.print(r)
			last = r.Time
		}
	}
}

// parseSince reads --since: a length of time back from now (90s, 10m, 2h,
// 3d), or a time, in RFC 3339 or the shorter forms people type, taken as
// local time when it has no zone.
func parseSince(s string, now time.Time) (time.Time, error) {
	if strings.HasSuffix(s, "d") {
		var days float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &days); err == nil && days >= 0 {
			return now.Add(-time.Duration(days * float64(24*time.Hour))), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q: use a length of time such as 10m or 2h, or a time such as 2026-11-18T09:00", s)
}

type logStyles struct {
	time, svc, err, field lipgloss.Style
	level                 map[string]lipgloss.Style
}

// newLogStyles colours for w: plain when w isn't a terminal, so piped
// output is clean text.
func newLogStyles(w io.Writer) logStyles {
	r := lipgloss.NewRenderer(w)
	return logStyles{
		time:  r.NewStyle().Faint(true),
		svc:   r.NewStyle().Foreground(lipgloss.Color("6")),
		err:   r.NewStyle().Foreground(lipgloss.Color("1")),
		field: r.NewStyle().Faint(true),
		level: map[string]lipgloss.Style{
			applog.LevelDebug: r.NewStyle().Foreground(lipgloss.Color("8")),
			applog.LevelInfo:  r.NewStyle().Foreground(lipgloss.Color("4")),
			applog.LevelWarn:  r.NewStyle().Foreground(lipgloss.Color("3")).Bold(true),
			applog.LevelError: r.NewStyle().Foreground(lipgloss.Color("1")).Bold(true),
		},
	}
}

type logPrinter struct {
	out       io.Writer
	json, raw bool
	all       bool
	style     logStyles
}

var levelTag = map[string]string{
	applog.LevelDebug: "DEBU", applog.LevelInfo: "INFO", applog.LevelWarn: "WARN", applog.LevelError: "ERRO",
}

func (p logPrinter) print(r applog.Record) {
	switch {
	case p.json:
		b, err := json.Marshal(r)
		if err != nil {
			return // can't happen: a Record is plain data
		}
		fmt.Fprintln(p.out, string(b))
		return
	case p.raw:
		if p.all {
			fmt.Fprintf(p.out, "%s: ", applog.Where(r))
		}
		fmt.Fprintln(p.out, r.Line)
		return
	}
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(p.style.time.Render(r.Time.Local().Format("2006-01-02 15:04:05")) + " ")
	}
	if p.all {
		b.WriteString(p.style.svc.Render(applog.Where(r)) + " ")
	}
	if tag, ok := levelTag[r.Level]; ok {
		b.WriteString(p.style.level[r.Level].Render(tag) + " ")
	} else if r.Stream == applog.Stderr {
		// No level, but it came on stderr: say so, as the one hint there is.
		b.WriteString(p.style.err.Render("err ") + " ")
	} else if r.Stream != "" {
		b.WriteString("     ")
	}
	b.WriteString(r.Message)
	if len(r.Fields) > 0 {
		keys := make([]string, 0, len(r.Fields))
		for k := range r.Fields {
			if k == "time" || k == "ts" || k == "timestamp" {
				continue // the line's own time; Outpost's is shown
			}
			if r.Source == applog.SourceAccess && (k == "method" || k == "host" || k == "path" || k == "status") {
				continue // a request's message says these already
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(" " + p.style.field.Render(k+"="+quoteIfSpaced(r.Fields[k])))
		}
	}
	fmt.Fprintln(p.out, b.String())
}

func quoteIfSpaced(v string) string {
	if strings.ContainsAny(v, " \t\"") {
		b, _ := json.Marshal(v)
		return string(b)
	}
	return v
}

// ownLogs shows, and with follow keeps showing, one of Outpost's own logs,
// keeping the lines about service when one is named.
func (a *app) ownLogs(source, service string, q applog.Query, follow bool, p logPrinter) error {
	rd := applog.Reader{Dir: a.logsDir(), Services: func(context.Context) ([]string, error) {
		services, err := manifest.LoadDir(a.servicesDir())
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(services))
		for _, s := range services {
			names = append(names, s.Name)
		}
		return names, nil
	}}
	if c := a.client(); c.Available() {
		rd.Events = func(_ context.Context, service string, limit int) ([]applog.Record, error) {
			evs, err := c.Events(service, limit)
			return eventRecords(evs), err
		}
	} else if source == applog.SourceEvents {
		return errNoDaemon()
	} else if source == applog.SourceAll {
		fmt.Fprintln(a.err, "logs: the daemon isn't running, so the activity feed is left out")
	}
	ctx := context.Background()
	recs, err := rd.Read(ctx, source, service, q)
	if err != nil {
		return err
	}
	for _, r := range recs {
		p.print(r)
	}
	if !follow {
		return nil
	}
	// Following asks again for what's at or after the newest line shown.
	// The daemon's own lines are stamped to the second, so a line written
	// in that second after the last read has the same time as the newest
	// shown; the lines already shown at that time are remembered, and only
	// the others printed.
	last := time.Now()
	seen := map[string]bool{}
	if n := len(recs); n > 0 && !recs[n-1].Time.IsZero() {
		last = recs[n-1].Time
		for _, r := range recs {
			if r.Time.Equal(last) {
				seen[r.Source+"\x00"+r.Line] = true
			}
		}
	}
	for {
		time.Sleep(time.Second)
		fq := q
		fq.Since, fq.Lines = last.Add(-time.Nanosecond), applog.MaxLines
		more, err := rd.Read(ctx, source, service, fq)
		if err != nil {
			return err
		}
		for _, r := range more {
			key := r.Source + "\x00" + r.Line
			if r.Time.Equal(last) && seen[key] {
				continue
			}
			if r.Time.After(last) {
				last, seen = r.Time, map[string]bool{}
			}
			seen[key] = true
			p.print(r)
		}
	}
}

// showEvents prints a service's recent lifecycle events.
func (a *app) showEvents(name string, limit int) error {
	c := a.client()
	if !c.Available() {
		return errNoDaemon()
	}
	events, err := c.Events(name, limit)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintf(a.out, "no recorded activity for %s\n", name)
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		fmt.Fprintf(a.out, "%s  %-12s %s\n",
			e.At.Format("2006-01-02 15:04:05"), e.Kind, e.Message)
	}
	return nil
}

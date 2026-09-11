// Package state is Outpost's local record of observed reality: what is running,
// what state it is in, and what has happened recently.
//
// The manifest, not this store, is the source of truth for which services exist
// (§8). Deleting state.db must be recoverable by re-running `outpost apply`.
// The one thing here that is *not* a cache is desired run state: `outpost stop`
// records intent a running supervisor reconciles toward.
package state

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Desired run state for a service.
const (
	DesiredRunning = "running"
	DesiredStopped = "stopped"
)

// Event kinds recorded in the activity feed.
const (
	EventStarted    = "started"
	EventStopped    = "stopped"
	EventRestarted  = "restarted"
	EventExited     = "exited"
	EventHealthFail = "health-fail"
	EventHealthOK   = "health-ok"
	EventError      = "error"
	EventApply      = "apply"
	EventBackup     = "backup"
	EventRestore    = "restore"
	// EventTailnet records a change in a service's Tailscale presence worth
	// an operator's attention — needing authorisation, or failing to attach.
	// Not the ordinary attached case: a crash-looping service already writes
	// started and exited rows, and two more per restart would drown the feed.
	EventTailnet = "tailnet"
)

// ServiceRecord is one row of observed service state.
type ServiceRecord struct {
	Name         string
	Runtime      string
	Status       string
	InstanceID   string // PID for processes, container ID for containers
	Desired      string
	RestartEpoch int64
	RestartCount int
	// Halted is set when the supervisor has given up on a service per its
	// restart_policy. Only an explicit start or restart clears it.
	Halted        bool
	LastStartedAt time.Time
	UpdatedAt     time.Time
}

// Event is one entry in the rolling activity log.
type Event struct {
	ID      int64
	Service string
	Kind    string
	Message string
	At      time.Time
}

// Store is a SQLite-backed state store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS services (
    name                 TEXT PRIMARY KEY,
    runtime              TEXT NOT NULL DEFAULT '',
    status               TEXT NOT NULL DEFAULT '',
    pid_or_container_id  TEXT NOT NULL DEFAULT '',
    desired              TEXT NOT NULL DEFAULT 'running',
    restart_epoch        INTEGER NOT NULL DEFAULT 0,
    restart_count        INTEGER NOT NULL DEFAULT 0,
    halted               INTEGER NOT NULL DEFAULT 0,
    last_started_at      INTEGER NOT NULL DEFAULT 0,
    updated_at           INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS events (
    id      INTEGER PRIMARY KEY AUTOINCREMENT,
    service TEXT NOT NULL,
    kind    TEXT NOT NULL,
    message TEXT NOT NULL DEFAULT '',
    at      INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS events_service_at ON events(service, at DESC);
CREATE INDEX IF NOT EXISTS events_at ON events(at DESC);
`

// Open opens (creating if needed) the state database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create state dir: %w", err)
		}
	}

	// A single supervisor writes and the CLI reads; WAL keeps readers from
	// blocking on the writer, and busy_timeout absorbs the overlap.
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open state db %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// UpsertService records observed state, leaving desired state and the restart
// epoch — which belong to the CLI, not the supervisor — untouched.
func (s *Store) UpsertService(rec ServiceRecord) error {
	now := time.Now()
	_, err := s.db.Exec(`
INSERT INTO services (name, runtime, status, pid_or_container_id, restart_count, last_started_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    runtime             = excluded.runtime,
    status              = excluded.status,
    pid_or_container_id = excluded.pid_or_container_id,
    restart_count       = excluded.restart_count,
    last_started_at     = excluded.last_started_at,
    updated_at          = excluded.updated_at`,
		rec.Name, rec.Runtime, rec.Status, rec.InstanceID,
		rec.RestartCount, unixOrZero(rec.LastStartedAt), now.Unix())
	if err != nil {
		return fmt.Errorf("upsert service %s: %w", rec.Name, err)
	}
	return nil
}

// SetDesired records that a service should be running or stopped, creating the
// row if the supervisor has not observed the service yet.
func (s *Store) SetDesired(name, desired string) error {
	if desired != DesiredRunning && desired != DesiredStopped {
		return fmt.Errorf("invalid desired state %q", desired)
	}
	// Asking for a service to run overrides the supervisor having given up on
	// it, so an explicit start always clears the halt.
	halted := 0
	_, err := s.db.Exec(`
INSERT INTO services (name, desired, halted, updated_at) VALUES (?, ?, ?, ?)
ON CONFLICT(name) DO UPDATE SET
    desired    = excluded.desired,
    halted     = CASE WHEN excluded.desired = 'running' THEN 0 ELSE services.halted END,
    updated_at = excluded.updated_at`,
		name, desired, halted, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set desired for %s: %w", name, err)
	}
	return nil
}

// RequestRestart bumps the restart epoch. A supervisor that sees an epoch newer
// than the one it last acted on restarts the service.
func (s *Store) RequestRestart(name string) error {
	_, err := s.db.Exec(`
INSERT INTO services (name, desired, restart_epoch, halted, updated_at) VALUES (?, ?, 1, 0, ?)
ON CONFLICT(name) DO UPDATE SET
    desired       = 'running',
    restart_epoch = services.restart_epoch + 1,
    halted        = 0,
    updated_at    = excluded.updated_at`,
		name, DesiredRunning, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("request restart for %s: %w", name, err)
	}
	return nil
}

// SetHalted records that the supervisor has stopped trying to keep a service
// running, so `outpost status` can explain why nothing is happening.
func (s *Store) SetHalted(name string, halted bool) error {
	_, err := s.db.Exec(`
INSERT INTO services (name, halted, updated_at) VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET halted = excluded.halted, updated_at = excluded.updated_at`,
		name, halted, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set halted for %s: %w", name, err)
	}
	return nil
}

const serviceCols = `name, runtime, status, pid_or_container_id, desired, restart_epoch, restart_count, halted, last_started_at, updated_at`

// GetService returns one record; ok is false when no such service is recorded.
func (s *Store) GetService(name string) (ServiceRecord, bool, error) {
	row := s.db.QueryRow(`SELECT `+serviceCols+` FROM services WHERE name = ?`, name)
	rec, err := scanService(row)
	if err == sql.ErrNoRows {
		return ServiceRecord{}, false, nil
	}
	if err != nil {
		return ServiceRecord{}, false, fmt.Errorf("get service %s: %w", name, err)
	}
	return rec, true, nil
}

// ListServices returns every recorded service, ordered by name.
func (s *Store) ListServices() ([]ServiceRecord, error) {
	rows, err := s.db.Query(`SELECT ` + serviceCols + ` FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var out []ServiceRecord
	for rows.Next() {
		rec, err := scanService(rows)
		if err != nil {
			return nil, fmt.Errorf("list services: %w", err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// DeleteService drops a service and its events, for services removed from the
// manifest.
func (s *Store) DeleteService(name string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	// Rollback after a successful Commit is a no-op error; nothing to do with it.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`DELETE FROM services WHERE name = ?`, name); err != nil {
		return fmt.Errorf("delete service %s: %w", name, err)
	}
	if _, err := tx.Exec(`DELETE FROM events WHERE service = ?`, name); err != nil {
		return fmt.Errorf("delete events for %s: %w", name, err)
	}
	return tx.Commit()
}

// AppendEvent records one activity-feed entry.
func (s *Store) AppendEvent(service, kind, message string) error {
	return s.appendEventAt(service, kind, message, time.Now())
}

func (s *Store) appendEventAt(service, kind, message string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO events (service, kind, message, at) VALUES (?, ?, ?, ?)`,
		service, kind, message, at.UnixMilli())
	if err != nil {
		return fmt.Errorf("append event for %s: %w", service, err)
	}
	return nil
}

// Events returns the most recent events first. An empty service means all
// services; limit <= 0 means no limit.
func (s *Store) Events(service string, limit int) ([]Event, error) {
	query := `SELECT id, service, kind, message, at FROM events`
	var args []any
	if service != "" {
		query += ` WHERE service = ?`
		args = append(args, service)
	}
	query += ` ORDER BY at DESC, id DESC`
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("read events: %w", err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var e Event
		var at int64
		if err := rows.Scan(&e.ID, &e.Service, &e.Kind, &e.Message, &at); err != nil {
			return nil, fmt.Errorf("read events: %w", err)
		}
		e.At = time.UnixMilli(at)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneEvents bounds the activity log by age and by total row count (§8).
func (s *Store) PruneEvents(maxAge time.Duration, maxRows int) error {
	if maxAge > 0 {
		cutoff := time.Now().Add(-maxAge).UnixMilli()
		if _, err := s.db.Exec(`DELETE FROM events WHERE at < ?`, cutoff); err != nil {
			return fmt.Errorf("prune events by age: %w", err)
		}
	}
	if maxRows > 0 {
		if _, err := s.db.Exec(`
DELETE FROM events WHERE id NOT IN (
    SELECT id FROM events ORDER BY at DESC, id DESC LIMIT ?
)`, maxRows); err != nil {
			return fmt.Errorf("prune events by count: %w", err)
		}
	}
	return nil
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface{ Scan(dest ...any) error }

func scanService(sc scanner) (ServiceRecord, error) {
	var rec ServiceRecord
	var lastStarted, updated int64
	err := sc.Scan(&rec.Name, &rec.Runtime, &rec.Status, &rec.InstanceID,
		&rec.Desired, &rec.RestartEpoch, &rec.RestartCount, &rec.Halted, &lastStarted, &updated)
	if err != nil {
		return ServiceRecord{}, err
	}
	if lastStarted > 0 {
		rec.LastStartedAt = time.Unix(lastStarted, 0)
	}
	if updated > 0 {
		rec.UpdatedAt = time.Unix(updated, 0)
	}
	return rec, nil
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

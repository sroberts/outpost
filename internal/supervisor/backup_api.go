package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/state"
)

// Backup and restore run on the reconcile-loop goroutine like every other API
// call, which is what makes "stop, archive, restart" safe: no tick can start
// the service back up while its storage is being read or replaced (§10).

// Backup stops a service, archives its storage, and restarts it if it was
// running. A service the user had already stopped stays stopped.
func (s *Supervisor) Backup(ctx context.Context, name string) (backup.Archive, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		svc, ok := s.desired[name]
		if !ok {
			return nil, fmt.Errorf("no such service %q", name)
		}
		if s.backupDir == "" {
			return nil, fmt.Errorf("no backup directory configured")
		}

		wasRunning := s.suspend(ctx, name)
		defer s.resume(ctx, name, wasRunning)

		// One timestamp for both the filename and what we report, so a backup
		// taken across a second boundary is still findable afterwards.
		now := time.Now()
		archive := backup.Archive{
			Service:   name,
			Timestamp: now.UTC().Format(backup.TimestampFormat),
		}
		archive.Path = backup.ArchivePath(s.backupDir, name, now)

		if err := backup.Create(svc.ExpandStorage(), archive.Path); err != nil {
			s.event(name, state.EventError, fmt.Sprintf("backup failed: %v", err))
			return nil, err
		}
		s.event(name, state.EventBackup, "backed up to "+archive.Path)
		s.logf("backed up %s to %s", name, archive.Path)

		// Report the archive as it landed on disk.
		return backup.Find(s.backupDir, name, archive.Timestamp)
	})
	if err != nil {
		return backup.Archive{}, err
	}
	return v.(backup.Archive), nil
}

// Restore stops a service, replaces its storage from an archive, and restarts
// it if it was running. An empty timestamp restores the most recent backup.
func (s *Supervisor) Restore(ctx context.Context, name, timestamp string) (backup.Archive, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		svc, ok := s.desired[name]
		if !ok {
			return nil, fmt.Errorf("no such service %q", name)
		}
		if s.backupDir == "" {
			return nil, fmt.Errorf("no backup directory configured")
		}

		archive, err := backup.Find(s.backupDir, name, timestamp)
		if err != nil {
			return nil, err
		}

		wasRunning := s.suspend(ctx, name)
		defer s.resume(ctx, name, wasRunning)

		if err := backup.Extract(archive.Path, svc.ExpandStorage()); err != nil {
			s.event(name, state.EventError, fmt.Sprintf("restore failed: %v", err))
			return nil, err
		}
		s.event(name, state.EventRestore, "restored from "+archive.Path)
		s.logf("restored %s from %s", name, archive.Path)
		return archive, nil
	})
	if err != nil {
		return backup.Archive{}, err
	}
	return v.(backup.Archive), nil
}

// ListBackups reports a service's archives, newest first.
func (s *Supervisor) ListBackups(ctx context.Context, name string) ([]backup.Archive, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		if s.backupDir == "" {
			return nil, fmt.Errorf("no backup directory configured")
		}
		return backup.List(s.backupDir, name)
	})
	if err != nil {
		return nil, err
	}
	return v.([]backup.Archive), nil
}

// BackupAll backs up every declared service, continuing past failures so one
// broken service does not skip the rest.
func (s *Supervisor) BackupAll(ctx context.Context) ([]backup.Archive, error) {
	// Fetch the service list through the loop, then back each one up in turn;
	// reading s.desired directly here would race the reconcile loop.
	names, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		svcs := s.servicesLocked()
		out := make([]string, 0, len(svcs))
		for _, svc := range svcs {
			out = append(out, svc.Name)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}

	var (
		archives []backup.Archive
		failures []string
	)
	for _, name := range names.([]string) {
		archive, err := s.Backup(ctx, name)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		archives = append(archives, archive)
	}
	if len(failures) > 0 {
		return archives, fmt.Errorf("%d backup(s) failed: %v", len(failures), failures)
	}
	return archives, nil
}

// suspend stops a running instance for the duration of a backup or restore,
// reporting whether it was running. Reconcile-loop goroutine only.
func (s *Supervisor) suspend(ctx context.Context, name string) bool {
	st, ok := s.tracked[name]
	if !ok || st.handle == nil {
		return false
	}
	s.stopInstance(ctx, st, "paused for backup")
	st.status = statusStopped
	s.persist(st)
	return true
}

// resume restarts a service suspended by suspend.
func (s *Supervisor) resume(ctx context.Context, name string, wasRunning bool) {
	if !wasRunning {
		return
	}
	st, ok := s.tracked[name]
	if !ok {
		return
	}
	// Clear any backoff: this stop was ours, not a crash.
	st.nextAttempt = time.Time{}
	st.healthFails = 0
	s.maybeStart(ctx, st)
}

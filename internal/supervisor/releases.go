package supervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

const (
	// releaseCheckEvery is how often each service's source is asked about. The
	// answer only ever prompts an operator, and it costs a GitHub request.
	releaseCheckEvery = 24 * time.Hour
	// releaseRetryEvery is how soon a check is made again after one that failed
	// or caught a release still uploading. A daemon usually starts as a machine
	// boots, often before the network is up, and shouldn't then know nothing for
	// a day; a release caught mid-upload would hide the one before it for as long.
	releaseRetryEvery = time.Hour
	// releaseCheckPoll is how often the checker looks for a check that's due.
	// It decides how soon an update is followed by a check of the new release,
	// and looking costs no more than an `outpost list`.
	releaseCheckPoll = 30 * time.Second
	// releaseCheckTimeout bounds asking about one service.
	releaseCheckTimeout = 20 * time.Second
)

// ReleaseCheck is what asking a service's source found.
type ReleaseCheck struct {
	Latest string // the latest release's tag
	Newer  bool   // Latest is worth installing over what's there
	Note   string // why it isn't offered, when there's more to say than "up to date"
	// Pending says the answer won't last: Latest's build is still uploading.
	Pending bool
}

// knownRelease is what the supervisor remembers about one service's latest
// release. Each answer holds for the source and release it was made against:
// once either changes it says nothing about what's there now.
type knownRelease struct {
	// The latest attempt, whether or not it worked.
	source, installed string
	attemptAt         time.Time
	failure           string

	// The latest answer, which outlives failed attempts.
	answer          ReleaseCheck
	answerSource    string
	answerInstalled string
	answeredAt      time.Time
}

// addRelease fills in what's known about svc's latest release. Reconcile-loop
// goroutine only.
func (s *Supervisor) addRelease(st *ServiceStatus, svc *manifest.Service) {
	k, ok := s.releases[svc.Name]
	if !ok {
		return
	}
	if k.answerSource == svc.Source && k.answerInstalled == svc.Release {
		st.LatestRelease = k.answer.Latest
		st.UpdateAvailable = k.answer.Newer
		st.ReleaseNote = k.answer.Note
		st.ReleaseCheckedAt = k.answeredAt
	}
	if k.source == svc.Source && k.installed == svc.Release {
		st.ReleaseCheckError = k.failure
	}
}

// RunReleaseChecks keeps the release cache current until ctx ends: each service
// from a source once a day, and again as soon as what's installed changes. It
// only ever finds things out. Nothing here installs anything.
//
// It runs apart from the reconcile loop, which must never wait on the network,
// and meets it only through submit. One service at a time is enough for
// something this infrequent, and is gentler on the rate limit.
func (s *Supervisor) RunReleaseChecks(ctx context.Context) {
	if s.checkRelease == nil {
		return
	}
	t := time.NewTicker(releaseCheckPoll)
	defer t.Stop()
	for {
		// An error here means the daemon is on its way down.
		if due, err := s.releaseChecksDue(ctx); err == nil {
			for _, svc := range due {
				// Recorded and logged by checkOneRelease, and all a failed
				// check means is trying again later.
				_, _ = s.checkOneRelease(ctx, svc)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// releaseChecksDue lists the services whose check has come round, as copies
// the checker can hold after leaving the loop, and forgets undeclared ones.
func (s *Supervisor) releaseChecksDue(ctx context.Context) ([]*manifest.Service, error) {
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		for name := range s.releases {
			if _, ok := s.desired[name]; !ok {
				delete(s.releases, name)
			}
		}
		var due []*manifest.Service
		for _, svc := range s.servicesLocked() {
			if svc.Source != "" && s.releaseCheckDue(svc) {
				c := *svc
				due = append(due, &c)
			}
		}
		return due, nil
	})
	if err != nil {
		return nil, err
	}
	return v.([]*manifest.Service), nil
}

// releaseCheckDue says whether svc should be asked about now. Reconcile-loop
// goroutine only.
func (s *Supervisor) releaseCheckDue(svc *manifest.Service) bool {
	k, ok := s.releases[svc.Name]
	if !ok || k.source != svc.Source || k.installed != svc.Release {
		return true
	}
	wait := releaseCheckEvery
	if k.failure != "" || k.answer.Pending {
		wait = releaseRetryEvery
	}
	return s.now().Sub(k.attemptAt) >= wait
}

// CheckRelease asks about one service's latest release now, for someone who
// wants an answer sooner than the daily check, and remembers it as that would.
func (s *Supervisor) CheckRelease(ctx context.Context, name string) (ReleaseCheck, error) {
	if s.checkRelease == nil {
		return ReleaseCheck{}, fmt.Errorf("release checks are not available")
	}
	v, err := s.submit(ctx, func(s *Supervisor) (any, error) {
		svc, ok := s.desired[name]
		if !ok {
			return nil, fmt.Errorf("no such service %q", name)
		}
		if svc.Source == "" {
			return nil, fmt.Errorf("%s has no source in its manifest", name)
		}
		c := *svc
		return &c, nil
	})
	if err != nil {
		return ReleaseCheck{}, err
	}
	return s.checkOneRelease(ctx, v.(*manifest.Service))
}

// checkOneRelease makes one check, off the loop, and hands the result to it.
func (s *Supervisor) checkOneRelease(ctx context.Context, svc *manifest.Service) (ReleaseCheck, error) {
	check, err := s.askSource(ctx, svc)
	if err != nil {
		s.logf("outpost: checking %s for a newer release: %v", svc.Name, err)
	}
	// A daemon shutting down has nowhere to keep the answer and no need of it.
	_, _ = s.submit(ctx, func(s *Supervisor) (any, error) {
		s.recordRelease(svc, check, err)
		return nil, nil
	})
	return check, err
}

// askSource bounds one check. It runs on a goroutine of its own, where a panic
// would end the process and every service with it, and a release check is
// never worth that.
func (s *Supervisor) askSource(ctx context.Context, svc *manifest.Service) (check ReleaseCheck, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("the check failed unexpectedly: %v", p)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, releaseCheckTimeout)
	defer cancel()
	return s.checkRelease(ctx, svc)
}

// recordRelease keeps a check's result. A failure leaves the last answer where
// it is. Reconcile-loop goroutine only.
func (s *Supervisor) recordRelease(svc *manifest.Service, check ReleaseCheck, err error) {
	k, ok := s.releases[svc.Name]
	if !ok {
		k = &knownRelease{}
		s.releases[svc.Name] = k
	}
	k.source, k.installed, k.attemptAt, k.failure = svc.Source, svc.Release, s.now(), ""
	if err != nil {
		k.failure = err.Error()
		return
	}
	k.answer, k.answerSource, k.answerInstalled, k.answeredAt = check, svc.Source, svc.Release, s.now()
}

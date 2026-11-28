// Package push sends notifications to people's phones and computers with
// standard Web Push: Safari on macOS, Home Screen web apps on iOS and iPadOS,
// and Chromium and Firefox, through one pipeline and with no Apple developer
// account.
//
// A device subscribes once, on the portal, and then gets Outpost's own
// notifications and those of every app its user can open and hasn't muted.
// Apps don't see subscriptions. Each is started with OUTPOST_PUSH_URL, a
// loopback endpoint this package serves, and OUTPOST_PUSH_TOKEN, its own, and
// names who a notification is for; Outpost decides which devices that means.
// Tapping one opens the portal, which sends the device on to the app, so a
// notification's link is always the subscription's own origin, as Declarative
// Web Push requires. See docs/design/push.md.
package push

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/apptoken"
)

// Environment variables every app is started with.
const (
	EnvURL   = "OUTPOST_PUSH_URL"
	EnvToken = "OUTPOST_PUSH_TOKEN"
)

// tokenPrefix marks an app's token for the push endpoint, so one found in a
// log says what it is.
const tokenPrefix = "opush_"

// Outpost is the sender name of Outpost's own notifications. No app can be
// called it: a service's name is never empty.
const Outpost = ""

// Errors Send returns besides a bad notification.
var (
	ErrNotReady = errors.New("notifications aren't available: the login gateway isn't running")
	ErrLimit    = fmt.Errorf("at most %d notifications an hour from one app", sendLimit)
)

// sendLimit bounds an app's sends in an hour, so one stuck in a loop can't
// keep everyone's phone buzzing; each user can mute it too.
const sendLimit = 300

// maxDevices bounds one person's subscriptions; past it, the oldest goes.
const maxDevices = 20

// maxAttempts is how many times a push service is asked to take one
// notification before it is given up on.
const maxAttempts = 3

// Gate is what Send needs from the gateway: who may receive what, whether a
// device is still signed in, and where tapping a notification goes.
type Gate interface {
	// MayReceive reports whether username may be sent service's
	// notifications; Outpost is Outpost's own.
	MayReceive(service, username string) bool
	// SignIns lists username's live sign-ins by ID; an error means none
	// can be checked now, not that there are none.
	SignIns(username string) (map[string]bool, error)
	// Navigate is the absolute URL, on the portal, that a notification
	// from service about path opens.
	Navigate(service, path string) string
}

// Options configures a Service.
type Options struct {
	// Path is push.db.
	Path string
	// Listen is the loopback address the endpoint serves on, which apps are
	// told as OUTPOST_PUSH_URL.
	Listen string
	// Subject is the VAPID contact a push service can reach the operator
	// at: a mailto: or https: URI.
	Subject string
	// Logf receives a line per subscription change and per failed delivery.
	Logf func(format string, v ...any)
}

// Service holds the subscriptions and sends to them. It is safe for
// concurrent use.
type Service struct {
	store    *store
	key      vapidKey
	tokenKey []byte
	listen   string
	subject  string
	logf     func(format string, v ...any)
	now      func() time.Time
	client   *http.Client
	// allow says whether an endpoint is a push service's, and whose.
	allow     func(*url.URL) (platform string, ok bool)
	queue     chan delivery
	retryBase time.Duration
	drain     time.Duration

	mu     sync.Mutex
	gate   Gate
	issued *apptoken.Issued
	sends  map[string][]time.Time // each app's recent sends, for sendLimit
	stats  Stats
}

// Stats counts deliveries since the daemon started.
type Stats struct {
	Delivered int `json:"delivered"`
	// Gone are subscriptions the push service said no longer exist, which
	// are forgotten.
	Gone int `json:"gone"`
	// Failed are deliveries given up on.
	Failed int `json:"failed"`
	// Dropped are deliveries that didn't fit the queue.
	Dropped int `json:"dropped"`
}

// Open opens push.db, making the VAPID key the first time; Serve starts the
// endpoint and Run delivers.
func Open(opts Options) (*Service, error) {
	if opts.Listen == "" || opts.Subject == "" {
		return nil, errors.New("push: Listen and Subject are required")
	}
	if err := ValidSubject(opts.Subject); err != nil {
		return nil, fmt.Errorf("push: %w", err)
	}
	st, err := openStore(opts.Path)
	if err != nil {
		return nil, err
	}
	key, err := st.vapidKey()
	if err == nil {
		var tk []byte
		if tk, err = st.randomKey("token_key"); err == nil {
			logf := opts.Logf
			if logf == nil {
				logf = func(string, ...any) {}
			}
			return &Service{
				store:    st,
				key:      key,
				tokenKey: tk,
				listen:   opts.Listen,
				subject:  opts.Subject,
				logf:     logf,
				now:      time.Now,
				client: &http.Client{
					Timeout: 30 * time.Second,
					// A push service answers; it doesn't send us elsewhere,
					// and following it could reach anything.
					CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
				},
				allow:     pushService,
				queue:     make(chan delivery, 1024),
				retryBase: 5 * time.Second,
				drain:     5 * time.Second,
				issued:    apptoken.NewIssued(tokenPrefix),
				sends:     map[string][]time.Time{},
			}, nil
		}
	}
	_ = st.close() // failing anyway
	return nil, err
}

// ValidSubject checks a VAPID contact: Apple refuses a request whose sub
// isn't a mailto: or https: URI.
func ValidSubject(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
	case u.Scheme == "mailto" && strings.Contains(u.Opaque, "@"):
		return nil
	case u.Scheme == "https" && u.Host != "":
		return nil
	}
	return fmt.Errorf("contact %q: want a mailto: or https: address, like mailto:you@example.com", s)
}

// Close closes push.db.
func (s *Service) Close() error { return s.store.close() }

// URL is what apps are told as OUTPOST_PUSH_URL.
func (s *Service) URL() string { return "http://" + s.listen }

// PublicKey is the VAPID public key browsers subscribe with.
func (s *Service) PublicKey() string { return s.key.public() }

// SetGate tells the Service who may receive what, once the gateway exists.
// Until then nothing is sent.
func (s *Service) SetGate(g Gate) {
	s.mu.Lock()
	s.gate = g
	s.mu.Unlock()
}

func (s *Service) currentGate() Gate {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gate
}

// Status counts deliveries since the daemon started.
func (s *Service) Status() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// Resolve gives an app its OUTPOST_PUSH_URL and OUTPOST_PUSH_TOKEN. The
// token is an HMAC of its name, so it is the same on every start and needs
// no revision of its own.
func (s *Service) Resolve(service string) (map[string]string, string) {
	tok := apptoken.Mint(tokenPrefix, s.tokenKey, service, 0)
	s.issued.Add(tok, service)
	return map[string]string{EnvURL: s.URL(), EnvToken: tok}, "push:0"
}

func (s *Service) serviceFor(tok string) (string, bool) { return s.issued.Service(tok) }

// pushService says whether u is a push service's endpoint, and whose. The
// endpoint comes from a browser, by way of a signed-in user, and the daemon
// POSTs to it, so only the push services browsers use are reached: anything
// else would let a user point the daemon at the machine's own network.
func pushService(u *url.URL) (string, bool) {
	if u.Scheme != "https" || u.Port() != "" || u.User != nil {
		return "", false
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case strings.HasSuffix(h, ".push.apple.com"):
		return "apple", true
	// Chromium builds without Chrome's branding are handed FCM endpoints on
	// jmt17.google.com rather than fcm.googleapis.com; it is the same service.
	case h == "fcm.googleapis.com", h == "jmt17.google.com":
		return "fcm", true
	case h == "updates.push.services.mozilla.com":
		return "mozilla", true
	case strings.HasSuffix(h, ".notify.windows.com"):
		return "windows", true
	}
	return "", false
}

// Subscribe keeps a browser's subscription for username, made from the
// sign-in signIn. It replaces one with the same endpoint.
func (s *Service) Subscribe(username, signIn, endpoint, p256dh, auth, userAgent string) error {
	if username == "" || signIn == "" {
		return errors.New("push: a user and a sign-in are required")
	}
	u, err := url.Parse(endpoint)
	if err != nil || len(endpoint) > 2048 {
		return bad("that isn't a push service's address")
	}
	platform, ok := s.allow(u)
	if !ok {
		return bad("notifications go through Apple's, Google's, Mozilla's or Microsoft's push service, and that address isn't one of them")
	}
	if k, err := decodeKey(p256dh); err != nil || len(k) != 65 || k[0] != 4 {
		return bad("the subscription's p256dh key isn't a P-256 public key")
	}
	if k, err := decodeKey(auth); err != nil || len(k) != 16 {
		return bad("the subscription's auth secret isn't 16 bytes")
	}
	if len(userAgent) > 512 {
		userAgent = userAgent[:512]
	}
	if _, err := s.store.upsert(Subscription{Username: username, SignIn: signIn, Endpoint: endpoint,
		P256dh: p256dh, Auth: auth, UserAgent: userAgent, Platform: platform}); err != nil {
		return err
	}
	subs, err := s.store.ofUser(username)
	if err != nil {
		return err
	}
	for len(subs) > maxDevices {
		if _, err := s.store.remove("", subs[0].ID); err != nil {
			return err
		}
		subs = subs[1:]
	}
	s.logf("push: %s turned on notifications on a %s device", username, platform)
	return nil
}

// Unsubscribe forgets username's subscription at endpoint, as a browser
// turning notifications off asks.
func (s *Service) Unsubscribe(username, endpoint string) error {
	return s.store.removeEndpoint(username, endpoint)
}

// Device is a subscription as its owner sees it: no keys, and no endpoint,
// which is as good as one.
type Device struct {
	ID          string
	SignIn      string
	UserAgent   string
	Platform    string
	CreatedAt   time.Time
	LastSuccess time.Time
	Failing     bool
}

// Devices lists username's subscriptions, oldest first.
func (s *Service) Devices(username string) ([]Device, error) {
	subs, err := s.store.ofUser(username)
	if err != nil {
		return nil, err
	}
	out := make([]Device, 0, len(subs))
	for _, sub := range subs {
		out = append(out, Device{ID: sub.ID, SignIn: sub.SignIn, UserAgent: sub.UserAgent, Platform: sub.Platform,
			CreatedAt: sub.CreatedAt, LastSuccess: sub.LastSuccess, Failing: sub.FailureCount > 0})
	}
	return out, nil
}

// Forget removes one of username's subscriptions; it reports whether there
// was one.
func (s *Service) Forget(username, id string) (bool, error) {
	ok, err := s.store.remove(username, id)
	if ok {
		s.logf("push: %s turned off notifications on a device", username)
	}
	return ok, err
}

// Muted lists the senders username has turned off; Outpost is Outpost's.
func (s *Service) Muted(username string) (map[string]bool, error) { return s.store.muted(username) }

// SetMuted turns service's notifications off or on for username.
func (s *Service) SetMuted(username, service string, muted bool) error {
	return s.store.setMuted(username, service, muted)
}

// limit counts a send against from's hourly allowance. Outpost's own aren't
// counted: they are few, and an admin's test is theirs to repeat.
func (s *Service) limit(from string) error {
	if from == Outpost {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	recent := slices.DeleteFunc(s.sends[from], func(t time.Time) bool { return now.Sub(t) >= time.Hour })
	if len(recent) >= sendLimit {
		s.sends[from] = recent
		return ErrLimit
	}
	s.sends[from] = append(recent, now)
	return nil
}

// Send queues n from from for to's devices, or, with no one named, for
// everyone with a device who may receive from's notifications. Someone who
// can't open the app, has muted it, or has no device gets nothing, and that
// isn't an error. It reports how many devices it was queued for; delivery
// happens in Run.
func (s *Service) Send(from string, to []string, n Notification) (int, error) {
	if err := n.check(); err != nil {
		return 0, err
	}
	gate := s.currentGate()
	if gate == nil {
		return 0, ErrNotReady
	}
	if err := s.limit(from); err != nil {
		return 0, err
	}
	body, err := payload(n, gate.Navigate(from, n.Path), from)
	if err != nil {
		return 0, err
	}
	if len(to) == 0 {
		if to, err = s.store.users(); err != nil {
			return 0, err
		}
	}
	seen := map[string]bool{}
	queued := 0
	for _, user := range to {
		if user == "" || seen[user] {
			continue
		}
		seen[user] = true
		subs, err := s.recipients(gate, from, user)
		if err != nil {
			s.logf("push: %v", err)
			continue
		}
		for _, sub := range subs {
			if s.enqueue(delivery{sub: sub, body: body, ttl: n.TTL, urgency: n.Urgency, topic: n.Topic}) {
				queued++
			}
		}
	}
	return queued, nil
}

// recipients are user's devices that may get from's notification: none if
// they can't open it or have muted it, and only those still signed in. A
// device whose sign-in has ended is forgotten, so a lost phone that was
// signed out stops showing what arrives.
func (s *Service) recipients(gate Gate, from, user string) ([]Subscription, error) {
	if !gate.MayReceive(from, user) {
		return nil, nil
	}
	muted, err := s.store.muted(user)
	if err != nil {
		return nil, fmt.Errorf("read %s's muted apps: %w", user, err)
	}
	if muted[from] {
		return nil, nil
	}
	subs, err := s.store.ofUser(user)
	if err != nil || len(subs) == 0 {
		return nil, err
	}
	live, err := gate.SignIns(user)
	if err != nil {
		return nil, fmt.Errorf("check %s's sign-ins: %w", user, err)
	}
	var out []Subscription
	for _, sub := range subs {
		if live[sub.SignIn] {
			out = append(out, sub)
			continue
		}
		if _, err := s.store.remove("", sub.ID); err != nil {
			return nil, err
		}
		s.logf("push: forgot one of %s's devices: it was signed out", user)
	}
	return out, nil
}

// delivery is one notification for one device.
type delivery struct {
	sub     Subscription
	body    []byte // the plaintext; each attempt seals it afresh
	ttl     int
	urgency string
	topic   string
	attempt int
}

func (s *Service) enqueue(d delivery) bool {
	select {
	case s.queue <- d:
		return true
	default:
		s.mu.Lock()
		s.stats.Dropped++
		s.mu.Unlock()
		s.logf("push: the delivery queue is full; a notification for %s was dropped", d.sub.Username)
		return false
	}
}

// workers is how many deliveries are under way at once.
const workers = 4

// Run delivers queued notifications until ctx ends. Deliveries still queued
// then are dropped: a notification is for now, and a restart is brief.
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case d := <-s.queue:
					s.deliver(ctx, d)
				}
			}
		})
	}
	wg.Wait()
}

// deliver hands d to its push service and acts on the answer as RFC 8030
// lays out: gone is forgotten, busy is tried again, and a refusal is logged,
// since it means the request or the key is wrong, not the device.
func (s *Service) deliver(ctx context.Context, d delivery) {
	retry, after := s.attempt(ctx, d)
	if !retry || ctx.Err() != nil {
		return
	}
	d.attempt++
	if d.attempt >= maxAttempts {
		s.count(func(st *Stats) { st.Failed++ })
		s.logf("push: gave up on a notification for %s's %s device after %d tries", d.sub.Username, d.sub.Platform, maxAttempts)
		return
	}
	if after <= 0 {
		after = s.retryBase << (d.attempt - 1)
	}
	go func() {
		t := time.NewTimer(after)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			s.enqueue(d)
		}
	}()
}

func (s *Service) count(f func(*Stats)) {
	s.mu.Lock()
	f(&s.stats)
	s.mu.Unlock()
}

// maxRetryAfter caps how long a push service's Retry-After is honoured.
const maxRetryAfter = 10 * time.Minute

// attempt makes one request, and says whether to try again and when.
func (s *Service) attempt(ctx context.Context, d delivery) (retry bool, after time.Duration) {
	who := d.sub.Username + "'s " + d.sub.Platform + " device"
	endpoint, err := url.Parse(d.sub.Endpoint)
	if err != nil {
		return false, 0
	}
	if _, ok := s.allow(endpoint); !ok {
		// Kept from before the rule, or tampered with: never reached.
		_, _ = s.store.remove("", d.sub.ID) // it's being dropped either way
		return false, 0
	}
	p256dh, err1 := decodeKey(d.sub.P256dh)
	auth, err2 := decodeKey(d.sub.Auth)
	if err1 != nil || err2 != nil {
		return false, 0
	}
	body, err := encrypt(d.body, p256dh, auth)
	if err != nil {
		s.logf("push: seal a notification for %s: %v", who, err)
		return false, 0
	}
	vapid, err := s.key.authorization(endpoint, s.subject, s.now())
	if err != nil {
		s.logf("push: sign a request for %s: %v", who, err)
		return false, 0
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.sub.Endpoint, strings.NewReader(string(body)))
	if err != nil {
		return false, 0
	}
	h := req.Header
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Encoding", "aes128gcm")
	h.Set("TTL", strconv.Itoa(d.ttl))
	h.Set("Urgency", d.urgency)
	if d.topic != "" {
		h.Set("Topic", d.topic)
	}
	h.Set("Authorization", vapid)

	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, 0
		}
		s.noteFailure(d.sub.ID)
		s.logf("push: couldn't reach the push service for %s: %v", who, redact(err, d.sub.Endpoint))
		return true, 0
	}
	// The answer is a few words at most; read some so the connection can
	// be kept, and no more.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close() // read-only

	switch code := resp.StatusCode; {
	case code >= 200 && code < 300:
		if err := s.store.succeeded(d.sub.ID); err != nil {
			s.logf("push: %v", err)
		}
		s.count(func(st *Stats) { st.Delivered++ })
		return false, 0
	case code == http.StatusNotFound || code == http.StatusGone:
		if _, err := s.store.remove("", d.sub.ID); err != nil {
			s.logf("push: %v", err)
		}
		s.count(func(st *Stats) { st.Gone++ })
		s.logf("push: forgot one of %s's devices: its push service says the subscription has ended", d.sub.Username)
		return false, 0
	case code == http.StatusTooManyRequests:
		return true, retryAfter(resp.Header.Get("Retry-After"), s.now())
	case code >= 500:
		s.noteFailure(d.sub.ID)
		return true, retryAfter(resp.Header.Get("Retry-After"), s.now())
	case code == http.StatusRequestEntityTooLarge:
		s.logf("push: the push service for %s says the notification is too large", who)
	case code == http.StatusBadRequest || code == http.StatusForbidden || code == http.StatusUnauthorized:
		s.logf("push: the push service for %s refused the request (%d): the VAPID key or contact (%s) may be wrong", who, code, s.subject)
	default:
		s.logf("push: the push service for %s answered %d", who, code)
	}
	s.count(func(st *Stats) { st.Failed++ })
	return false, 0
}

func (s *Service) noteFailure(id string) {
	if err := s.store.failed(id); err != nil {
		s.logf("push: %v", err)
	}
}

// retryAfter reads a Retry-After header, seconds or a date, capped.
func retryAfter(v string, now time.Time) time.Duration {
	var d time.Duration
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
		d = time.Duration(n) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	if d > maxRetryAfter {
		d = maxRetryAfter
	}
	if d < 0 {
		d = 0
	}
	return d
}

// redact takes the endpoint out of an error, since it is as good as the
// subscription.
func redact(err error, endpoint string) string {
	msg := err.Error()
	if u, perr := url.Parse(endpoint); perr == nil {
		msg = strings.ReplaceAll(msg, endpoint, u.Scheme+"://"+u.Host+"/…")
		msg = strings.ReplaceAll(msg, strconv.Quote(endpoint), u.Scheme+"://"+u.Host+"/…")
	}
	return msg
}

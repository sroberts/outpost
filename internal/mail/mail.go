// Package mail gives Outpost's apps email through one Gmail account, each at
// its own plus address: mail to you+yarr@gmail.com is Yarr's, and so is mail
// to you+k7q2@gmail.com when k7q2 is an alias Yarr was given. Outpost reads
// the inbox over IMAP, keeps each routed message for its app, labels it
// Outpost/<app> in Gmail and archives it. Mail to no route is never touched.
//
// Apps don't hold the account's password. Each is started with
// OUTPOST_MAIL_URL, a loopback endpoint this package serves, and
// OUTPOST_MAIL_TOKEN, its own, and reads its messages and sends there. Each
// route also has an Atom feed, so a feed reader can subscribe to the mail
// sent to it. See docs/design/mail.md.
package mail

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	netmail "net/mail"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables every app is started with.
const (
	EnvURL   = "OUTPOST_MAIL_URL"
	EnvToken = "OUTPOST_MAIL_TOKEN"
)

// firstLook is how far back the first read of a new account goes, so mail
// sent to a route just before it was set up still arrives.
const firstLook = 7 * 24 * time.Hour

// maxBatch bounds the messages looked at in one check; the rest wait for the
// next, which follows at once.
const maxBatch = 200

// Keys is where the account's app password is kept: secrets.db, as a secret
// bound to no app.
type Keys interface {
	Value(name string) (string, bool)
	Put(name, value string) error
}

// Options configures a Service.
type Options struct {
	// Path is mail.db.
	Path string
	// Listen is the loopback address the endpoint serves on, which apps are
	// told as OUTPOST_MAIL_URL.
	Listen string
	// Address is config.toml's account, used until the portal sets one.
	Address string
	// Poll is how often the inbox is checked.
	Poll time.Duration
	// IMAP and SMTP are the servers, as host:port.
	IMAP, SMTP string
	// Keys holds the app password, under PasswordSecret.
	Keys           Keys
	PasswordSecret string
	// Services lists the declared apps, whose names are routes.
	Services func() []string
	// Logf receives one line per change and per routed message.
	Logf func(format string, v ...any)
}

// Service is the mail endpoint, the inbox reader and their settings. It is
// safe for concurrent use.
type Service struct {
	store    *store
	listen   string
	file     string
	poll     time.Duration
	imapAddr string
	smtpAddr string
	keys     Keys
	secret   string
	services func() []string
	logf     func(format string, v ...any)
	now      func() time.Time
	dial     dialer
	send     sender
	wake     chan struct{}

	mu sync.Mutex
	// issued maps a token's hash to the app it was handed to.
	issued map[[32]byte]string
	status Status

	// checking is held for a whole check, so two never overlap.
	checking sync.Mutex
}

// Status is how reading the inbox is going.
type Status struct {
	Address    string `json:"address,omitempty"`
	FromPortal bool   `json:"from_portal,omitempty"`
	// HasPassword says the app password is saved.
	HasPassword bool `json:"has_password"`
	// Checked is the last check that finished, and Error why it failed.
	Checked time.Time `json:"checked,omitzero"`
	Error   string    `json:"error,omitempty"`
	// LastRouted is when a message was last routed to an app.
	LastRouted time.Time `json:"last_routed,omitzero"`
	// Routed counts messages routed since the daemon started.
	Routed int `json:"routed"`
}

// Ready reports whether there's an account to read.
func (s Status) Ready() bool { return s.Address != "" && s.HasPassword }

// Open opens mail.db and readies the endpoint; Serve starts it, and Run
// reads the inbox.
func Open(opts Options) (*Service, error) {
	if opts.Listen == "" || opts.IMAP == "" || opts.SMTP == "" || opts.PasswordSecret == "" {
		return nil, errors.New("mail: Listen, IMAP, SMTP and PasswordSecret are required")
	}
	if opts.Address != "" {
		if _, err := parseAccount(opts.Address); err != nil {
			return nil, fmt.Errorf("mail: address %q: %w", opts.Address, err)
		}
	}
	st, err := openStore(opts.Path)
	if err != nil {
		return nil, err
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	services := opts.Services
	if services == nil {
		services = func() []string { return nil }
	}
	poll := opts.Poll
	if poll <= 0 {
		poll = time.Minute
	}
	return &Service{
		store:    st,
		listen:   opts.Listen,
		file:     opts.Address,
		poll:     poll,
		imapAddr: opts.IMAP,
		smtpAddr: opts.SMTP,
		keys:     opts.Keys,
		secret:   opts.PasswordSecret,
		services: services,
		logf:     logf,
		now:      time.Now,
		dial:     dialIMAP,
		send:     smtpSend,
		wake:     make(chan struct{}, 1),
		issued:   map[[32]byte]string{},
	}, nil
}

// Close closes mail.db.
func (s *Service) Close() error { return s.store.close() }

// URL is what apps are told as OUTPOST_MAIL_URL.
func (s *Service) URL() string { return "http://" + s.listen }

// SetServices tells the Service how to list the declared apps, once the
// supervisor that knows them exists.
func (s *Service) SetServices(f func() []string) {
	s.mu.Lock()
	s.services = f
	s.mu.Unlock()
}

func (s *Service) serviceNames() []string {
	s.mu.Lock()
	f := s.services
	s.mu.Unlock()
	return f()
}

// address is the account in force: the portal's, or config.toml's.
func (s *Service) address() (addr string, fromPortal bool) {
	if a := s.store.meta("address"); a != "" {
		return a, true
	}
	return s.file, false
}

func (s *Service) password() (string, bool) {
	if s.keys == nil {
		return "", false
	}
	v, ok := s.keys.Value(s.secret)
	return v, ok && v != ""
}

// Status reports the account and how reading it is going.
func (s *Service) Status() Status {
	s.mu.Lock()
	st := s.status
	s.mu.Unlock()
	st.Address, st.FromPortal = s.address()
	_, st.HasPassword = s.password()
	return st
}

// PasswordSecret is the secret the app password is kept in.
func (s *Service) PasswordSecret() string { return s.secret }

// SetAccount sets the Gmail account. An empty password keeps the one saved.
// Gmail shows an app password in groups of four, so spaces are taken out.
func (s *Service) SetAccount(address, password, by string) error {
	address = strings.TrimSpace(address)
	if _, err := parseAccount(address); err != nil {
		return err
	}
	password = strings.ReplaceAll(strings.TrimSpace(password), " ", "")
	if password == "" {
		if _, ok := s.password(); !ok {
			return errors.New("the app password is required")
		}
	} else {
		if s.keys == nil {
			return errors.New("there's nowhere to keep the password")
		}
		if err := s.keys.Put(s.secret, password); err != nil {
			return err
		}
	}
	old, _ := s.address()
	if !strings.EqualFold(old, address) {
		// Another mailbox's UIDs mean nothing here.
		if err := s.store.forgetCursor(); err != nil {
			return err
		}
	}
	if err := s.store.setMeta("address", address); err != nil {
		return fmt.Errorf("save the address: %w", err)
	}
	s.mu.Lock()
	s.status.Error = ""
	s.mu.Unlock()
	s.logf("mail: %s set the account to %s", by, address)
	s.CheckNow()
	return nil
}

// CheckNow asks Run to check the inbox without waiting for the next poll.
func (s *Service) CheckNow() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Resolve gives an app its OUTPOST_MAIL_URL and OUTPOST_MAIL_TOKEN, and a
// revision that changes when its token does, so the supervisor restarts it
// with the new one.
func (s *Service) Resolve(service string) (map[string]string, string) {
	tok, gen := s.store.token(service)
	s.mu.Lock()
	s.issued[sha256.Sum256([]byte(tok))] = service
	s.mu.Unlock()
	return map[string]string{EnvURL: s.URL(), EnvToken: tok}, "mail:" + strconv.FormatInt(gen, 10)
}

// Regenerate gives service a new token. The old one stops working at once.
func (s *Service) Regenerate(service, by string) error {
	if service == "" {
		return errors.New("an app is required")
	}
	old, _ := s.store.token(service)
	if err := s.store.regenerate(service); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.issued, sha256.Sum256([]byte(old)))
	s.mu.Unlock()
	s.logf("mail: %s regenerated %s's token", by, service)
	return nil
}

func (s *Service) serviceFor(tok string) (string, bool) {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.issued[sha256.Sum256([]byte(tok))]
	return svc, ok
}

// Route is one address an app gets mail at.
type Route struct {
	Route   string `json:"route"`
	Service string `json:"-"`
	// Alias says it's a random one, which can be removed; otherwise it's
	// the app's name.
	Alias bool   `json:"alias"`
	Note  string `json:"note,omitempty"`
	// Address is the whole address, "" until there's an account.
	Address string `json:"address,omitempty"`
	// Feed is the Atom feed of the mail sent to it.
	Feed string `json:"feed"`
}

// Routes lists the addresses service gets mail at: its name, when that can
// be one, then its aliases, oldest first.
func (s *Service) Routes(service string) []Route {
	var acct *account
	if addr, _ := s.address(); addr != "" {
		if a, err := parseAccount(addr); err == nil {
			acct = &a
		}
	}
	mk := func(r, note string, alias bool) Route {
		rt := Route{Route: r, Service: service, Alias: alias, Note: note, Feed: s.feedURL(r)}
		if acct != nil {
			rt.Address = acct.address(r)
		}
		return rt
	}
	var out []Route
	if validRoute(service) {
		if _, taken := s.store.aliasFor(service); !taken {
			out = append(out, mk(service, "", false))
		}
	}
	aliases := s.store.aliasesOf(service)
	slices.SortFunc(aliases, func(a, b alias) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Alias, b.Alias)
	})
	for _, a := range aliases {
		out = append(out, mk(a.Alias, a.Note, true))
	}
	return out
}

// owner is the app a route belongs to: an alias's, or the app of that name.
func (s *Service) owner(route string) (string, bool) {
	return s.ownerIn(route, s.serviceNames)
}

// ownerIn is owner, listing the apps only if it has to.
func (s *Service) ownerIn(route string, names func() []string) (string, bool) {
	if a, ok := s.store.aliasFor(route); ok {
		return a.Service, true
	}
	if !validRoute(route) {
		return "", false
	}
	for _, name := range names() {
		if name == route {
			return name, true
		}
	}
	return "", false
}

// maxNote bounds an alias's note.
const maxNote = 80

// AddAlias gives service a new random address. The note says what it's
// for, such as the newsletter it was given to.
func (s *Service) AddAlias(service, note, by string) (Route, error) {
	if service == "" {
		return Route{}, errors.New("an app is required")
	}
	note = strings.TrimSpace(headerSafe(note))
	if len(note) > maxNote {
		return Route{}, fmt.Errorf("the note is at most %d characters", maxNote)
	}
	names := s.serviceNames()
	for range 20 {
		r, err := newAlias()
		if err != nil {
			return Route{}, err
		}
		if _, taken := s.store.aliasFor(r); taken || slices.Contains(names, r) {
			continue
		}
		if err := s.store.addAlias(alias{Alias: r, Service: service, Note: note, CreatedBy: by}); err != nil {
			return Route{}, err
		}
		s.logf("mail: %s gave %s the address +%s", by, service, r)
		for _, rt := range s.Routes(service) {
			if rt.Route == r {
				return rt, nil
			}
		}
	}
	return Route{}, errors.New("couldn't find a free address; try again")
}

// RemoveAlias takes an alias away from service. Mail to it is then left in
// the inbox, like any other mail to no route.
func (s *Service) RemoveAlias(service, route, by string) error {
	a, ok := s.store.aliasFor(route)
	if !ok || a.Service != service {
		return fmt.Errorf("%s has no address +%s", service, route)
	}
	if err := s.store.removeAlias(route); err != nil {
		return err
	}
	s.logf("mail: %s took the address +%s away from %s", by, route, service)
	return nil
}

// CanSend reports whether service may send mail.
func (s *Service) CanSend(service string) bool { return s.store.canSend(service) }

// SetSend allows or stops service sending mail.
func (s *Service) SetSend(service string, allowed bool, by string) error {
	if service == "" {
		return errors.New("an app is required")
	}
	if err := s.store.setSend(service, allowed, by); err != nil {
		return err
	}
	if allowed {
		s.logf("mail: %s let %s send mail", by, service)
	} else {
		s.logf("mail: %s stopped %s sending mail", by, service)
	}
	return nil
}

// Held is how many messages service has waiting, and when the newest came.
func (s *Service) Held(service string) (int, time.Time) { return s.store.held(service) }

// Run checks the inbox every poll, and when CheckNow asks, until ctx ends.
func (s *Service) Run(ctx context.Context) {
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		more := s.checkOnce(ctx)
		if more {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.wake:
		}
	}
}

// checkOnce runs one check and records how it went. It reports whether a
// batch was cut short, so another should follow at once.
func (s *Service) checkOnce(ctx context.Context) bool {
	if !s.Status().Ready() {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	routed, more, err := s.check(cctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Checked = s.now()
	if err != nil {
		if ctx.Err() != nil {
			return false
		}
		if s.status.Error != err.Error() {
			s.logf("mail: %v", err)
		}
		s.status.Error = err.Error()
		return false
	}
	s.status.Error = ""
	if routed > 0 {
		s.status.Routed += routed
		s.status.LastRouted = s.now()
	}
	return more
}

// check reads what's new in the inbox and routes it. Only the recipient
// headers of mail to no route are read; its body never is.
func (s *Service) check(ctx context.Context) (routed int, more bool, err error) {
	s.checking.Lock()
	defer s.checking.Unlock()
	addr, _ := s.address()
	acct, err := parseAccount(addr)
	if err != nil {
		return 0, false, err
	}
	pass, ok := s.password()
	if !ok {
		return 0, false, ErrNotReady
	}
	sess, err := s.dial(ctx, s.imapAddr, addr, pass)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = sess.close() }() // what was done is done; a failed logout loses nothing

	validity, err := sess.inbox()
	if err != nil {
		return 0, false, err
	}
	known, last := s.store.cursor()
	if known != validity {
		last = 0
	}
	uids, err := sess.search(last, s.now().Add(-firstLook))
	if err != nil {
		return 0, false, err
	}
	slices.Sort(uids)
	if len(uids) > maxBatch {
		uids, more = uids[:maxBatch], true
	}
	if len(uids) == 0 {
		return 0, false, s.store.setCursor(validity, last)
	}

	heads, err := sess.headers(uids)
	if err != nil {
		return 0, false, err
	}
	// The apps are listed once for the whole batch, and only if a route
	// isn't an alias.
	var names []string
	listed := false
	appNames := func() []string {
		if !listed {
			names, listed = s.serviceNames(), true
		}
		return names
	}
	type dest struct{ service, route, rcpt string }
	where := map[uint32]dest{}
	var want []uint32
	for _, uid := range uids {
		for _, rcpt := range recipients(heads[uid]) {
			route, ok := acct.route(rcpt)
			if !ok {
				continue
			}
			if svc, ok := s.ownerIn(route, appNames); ok {
				where[uid] = dest{svc, route, acct.address(route)}
				want = append(want, uid)
				break
			}
		}
	}
	bodies, err := sess.bodies(want)
	if err != nil {
		return 0, false, err
	}
	filed := map[string][]uint32{}
	for _, uid := range want {
		d := where[uid]
		raw, ok := bodies[uid]
		if !ok {
			continue // gone between the two fetches; nothing to keep
		}
		m, err := parse(raw)
		if err != nil {
			s.logf("mail: a message to +%s couldn't be read, so it's left in the inbox: %v", d.route, err)
			continue
		}
		m.service, m.Route, m.To, m.Received = d.service, d.route, d.rcpt, s.now().UTC()
		if m.Date.IsZero() {
			m.Date = m.Received
		}
		src := strconv.FormatUint(uint64(validity), 10) + ":" + strconv.FormatUint(uint64(uid), 10)
		isNew, err := s.store.put(src, m)
		if err != nil {
			// Not filed, and the cursor stays put: the next check tries
			// again.
			return routed, false, err
		}
		if isNew {
			routed++
			s.logf("mail: routed a message from %s to %s (+%s)", m.From.Address, d.service, d.route)
		}
		filed[d.service] = append(filed[d.service], uid)
	}
	// The cursor moves before filing: the messages are kept, and a label
	// that fails to apply leaves them in the inbox rather than routing them
	// twice.
	if err := s.store.setCursor(validity, uids[len(uids)-1]); err != nil {
		return routed, false, err
	}
	var fileErr error
	for _, svc := range slices.Sorted(maps.Keys(filed)) {
		if err := sess.file(filed[svc], "Outpost/"+svc); err != nil && fileErr == nil {
			fileErr = fmt.Errorf("mail for %s was kept, but couldn't be labelled and archived in Gmail: %w", svc, err)
		}
	}
	return routed, more, fileErr
}

// Messages lists service's messages after id, oldest first.
func (s *Service) Messages(service string, after int64, limit int) ([]Message, error) {
	return s.store.list(service, after, limit)
}

// Send sends a message from one of service's addresses. It returns the new
// message's Message-ID.
func (s *Service) Send(ctx context.Context, service string, o Outgoing) (string, error) {
	if !s.store.canSend(service) {
		return "", ErrNotAllowed
	}
	addr, _ := s.address()
	acct, err := parseAccount(addr)
	if err != nil {
		return "", ErrNotReady
	}
	pass, ok := s.password()
	if !ok {
		return "", ErrNotReady
	}
	route := strings.ToLower(o.Route)
	if route == "" {
		route = service
	}
	if owner, ok := s.owner(route); !ok || owner != service {
		return "", bad("route: %s has no address +%s", service, route)
	}
	var inReplyTo string
	if o.InReplyTo != 0 {
		orig, err := s.store.get(service, o.InReplyTo)
		if err != nil {
			return "", bad("in_reply_to: there's no message %d in this app's mail", o.InReplyTo)
		}
		inReplyTo = orig.MessageID
		if len(o.To) == 0 {
			o.To = []string{firstNonEmpty(orig.ReplyTo, orig.From.Address)}
		}
		if o.Subject == "" {
			o.Subject = orig.Subject
			if !strings.HasPrefix(strings.ToLower(o.Subject), "re:") {
				o.Subject = "Re: " + o.Subject
			}
		}
	}
	to, err := addresses("to", o.To)
	if err != nil {
		return "", err
	}
	cc, err := addresses("cc", o.Cc)
	if err != nil {
		return "", err
	}
	bcc, err := addresses("bcc", o.Bcc)
	if err != nil {
		return "", err
	}
	all := slices.Concat(to, cc, bcc)
	switch {
	case len(all) == 0:
		return "", bad("to: name at least one recipient")
	case len(all) > maxRecipients:
		return "", bad("at most %d recipients in all", maxRecipients)
	case strings.TrimSpace(o.Subject) == "":
		return "", bad("subject: required")
	case len(o.Subject) > maxSubject:
		return "", bad("subject: at most %d bytes", maxSubject)
	case o.Text == "" && o.HTML == "":
		return "", bad("give text, html or both")
	case len(o.Text)+len(o.HTML) > maxSendBody:
		return "", bad("the message is at most %d bytes", maxSendBody)
	}
	if s.store.sentSince(service, s.now().Add(-24*time.Hour)) >= dailyLimit {
		return "", ErrLimit
	}
	fromAddr := acct.address(route)
	from := &netmail.Address{Name: headerSafe(firstNonEmpty(o.FromName, service)), Address: fromAddr}
	msg, id, err := compose(from, fromAddr, to, cc, o, inReplyTo, s.now())
	if err != nil {
		return "", err
	}
	rcpts := make([]string, len(all))
	for i, a := range all {
		rcpts[i] = a.Address
	}
	// The envelope is the account's own address, which Gmail requires; the
	// From and Reply-To headers carry the route, so answers come back to it.
	if err := s.send(ctx, s.smtpAddr, addr, pass, addr, rcpts, msg); err != nil {
		return "", fmt.Errorf("gmail didn't take it: %w", err)
	}
	if err := s.store.recordSent(service, route, rcpts, o.Subject, id); err != nil {
		s.logf("mail: %s sent a message, but it wasn't recorded: %v", service, err)
	}
	s.logf("mail: %s sent a message from +%s to %d recipient(s)", service, route, len(rcpts))
	return id, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// Serve answers apps on ln until ctx ends.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      3 * time.Minute, // sending waits on Gmail
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut) // a request still open at exit is cut, which is all that's left to do
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

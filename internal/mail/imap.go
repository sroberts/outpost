package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// session is what reading the inbox needs of an IMAP connection. The poller
// is tested through it, with no server.
type session interface {
	// inbox selects INBOX, read-write, and reports its UIDVALIDITY.
	inbox() (uint32, error)
	// search lists the UIDs above after, or with after 0, of messages
	// received since since.
	search(after uint32, since time.Time) ([]uint32, error)
	// headers fetches the recipient headers of each message, without
	// marking it read.
	headers(uids []uint32) (map[uint32][]byte, error)
	// bodies fetches whole messages, without marking them read.
	bodies(uids []uint32) (map[uint32][]byte, error)
	// file gives messages a label and takes them out of the inbox.
	file(uids []uint32, label string) error
	close() error
}

// dialer opens a session, logged in.
type dialer func(ctx context.Context, addr, user, pass string) (session, error)

// imapTimeout bounds each command, so a server that stops answering can't
// hold the poller for ever.
const imapTimeout = 60 * time.Second

func dialIMAP(ctx context.Context, addr, user, pass string) (session, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return nil, fmt.Errorf("connect to %s: %w", addr, err)
	}
	return login(ctx, conn, user, pass)
}

// login starts a session on conn, which is closed if it fails.
func login(ctx context.Context, conn net.Conn, user, pass string) (session, error) {
	c, err := client.New(conn)
	if err != nil {
		_ = conn.Close() // failing anyway
		return nil, fmt.Errorf("start IMAP: %w", err)
	}
	c.Timeout = imapTimeout
	// The context bounds the whole session: cancelling it closes the
	// connection, which ends whatever command is waiting.
	stop := context.AfterFunc(ctx, func() { _ = c.Terminate() })
	if err := c.Login(user, pass); err != nil {
		stop()
		_ = c.Logout() // failing anyway
		return nil, loginError(err)
	}
	return &imapSession{c: c, stop: stop}, nil
}

// loginError says what a refused login usually means with Gmail.
func loginError(err error) error {
	return fmt.Errorf("gmail refused the login (%v); check the address, and that the password is an app password from myaccount.google.com/apppasswords, not the account's own", err)
}

type imapSession struct {
	c    *client.Client
	stop func() bool
}

func (s *imapSession) inbox() (uint32, error) {
	mbox, err := s.c.Select("INBOX", false)
	if err != nil {
		return 0, fmt.Errorf("open the inbox: %w", err)
	}
	return mbox.UidValidity, nil
}

func (s *imapSession) search(after uint32, since time.Time) ([]uint32, error) {
	crit := imap.NewSearchCriteria()
	if after > 0 {
		crit.Uid = new(imap.SeqSet)
		crit.Uid.AddRange(after+1, 0)
	} else {
		crit.Since = since
	}
	uids, err := s.c.UidSearch(crit)
	if err != nil {
		return nil, fmt.Errorf("search the inbox: %w", err)
	}
	// "n:*" always matches the newest message, even when n is past it.
	out := uids[:0]
	for _, u := range uids {
		if u > after {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *imapSession) fetch(uids []uint32, section *imap.BodySectionName) (map[uint32][]byte, error) {
	out := map[uint32][]byte{}
	if len(uids) == 0 {
		return out, nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	msgs := make(chan *imap.Message, 16)
	done := make(chan error, 1)
	go func() { done <- s.c.UidFetch(set, []imap.FetchItem{imap.FetchUid, section.FetchItem()}, msgs) }()
	for m := range msgs {
		r := m.GetBody(section)
		if r == nil {
			continue
		}
		var b bytes.Buffer
		if _, err := io.Copy(&b, r); err != nil {
			continue
		}
		out[m.Uid] = b.Bytes()
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("fetch from the inbox: %w", err)
	}
	return out, nil
}

func (s *imapSession) headers(uids []uint32) (map[uint32][]byte, error) {
	return s.fetch(uids, &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier,
			Fields: []string{"Delivered-To", "X-Original-To", "To", "Cc"}},
		Peek: true,
	})
}

func (s *imapSession) bodies(uids []uint32) (map[uint32][]byte, error) {
	return s.fetch(uids, &imap.BodySectionName{Peek: true})
}

// file uses Gmail's X-GM-LABELS: adding a label that doesn't exist yet makes
// it, and taking away \Inbox archives the message, which then leaves INBOX.
func (s *imapSession) file(uids []uint32, label string) error {
	if len(uids) == 0 {
		return nil
	}
	set := new(imap.SeqSet)
	set.AddNum(uids...)
	if err := s.c.UidStore(set, imap.StoreItem("+X-GM-LABELS.SILENT"), []interface{}{label}, nil); err != nil {
		return fmt.Errorf("label %s: %w", label, err)
	}
	if err := s.c.UidStore(set, imap.StoreItem("-X-GM-LABELS.SILENT"), []interface{}{imap.RawString(`\Inbox`)}, nil); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}

func (s *imapSession) close() error {
	defer s.stop()
	return s.c.Logout()
}

package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"
	"unicode"
)

// Sending's limits. Gmail's own is about 500 a day for the whole account,
// and past it the account can't send at all for a day, so each app gets a
// share well inside it.
const (
	maxRecipients = 20
	maxSubject    = 998
	maxSendBody   = 4 << 20
	dailyLimit    = 100
)

// Errors Send returns, which the endpoint turns into statuses.
var (
	ErrNotAllowed = errors.New("this app isn't allowed to send mail; an admin can allow it under the app's Manage menu, in Mail")
	ErrNotReady   = errors.New("no Gmail account is set up; an admin sets one on the portal's Mail page")
	ErrLimit      = fmt.Errorf("this app has sent %d messages in the last 24 hours, its limit", dailyLimit)
)

// Outgoing is a message an app asks to send.
type Outgoing struct {
	To  []string `json:"to"`
	Cc  []string `json:"cc,omitempty"`
	Bcc []string `json:"bcc,omitempty"`
	// Subject, Text and HTML; at least one of Text and HTML.
	Subject string `json:"subject"`
	Text    string `json:"text,omitempty"`
	HTML    string `json:"html,omitempty"`
	// FromName is the name shown beside the address; unset is the app's.
	FromName string `json:"from_name,omitempty"`
	// Route is which of the app's addresses it's from; unset is the one
	// named after the app.
	Route string `json:"route,omitempty"`
	// InReplyTo is the id of a message in the app's inbox this answers. It
	// threads the reply, and with no To, goes to whoever sent that one.
	InReplyTo int64 `json:"in_reply_to,omitempty"`
}

// badRequest is a problem with what the app sent, for a 400.
type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func bad(format string, v ...any) error { return badRequest{fmt.Sprintf(format, v...)} }

// sender delivers a composed message. Tests use fakes.
type sender func(ctx context.Context, addr, user, pass, from string, to []string, msg []byte) error

// addresses checks and normalises a list of recipients.
func addresses(field string, list []string) ([]*netmail.Address, error) {
	var out []*netmail.Address
	for _, s := range list {
		a, err := netmail.ParseAddress(s)
		if err != nil {
			return nil, bad("%s: %q isn't an email address", field, s)
		}
		out = append(out, a)
	}
	return out, nil
}

// headerSafe takes out what could end a header line or start another.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

// compose builds the message: headers, then the text and HTML as
// alternatives. bcc is left out of it, as it has to be.
func compose(from *netmail.Address, replyTo string, to, cc []*netmail.Address, o Outgoing, inReplyTo string, now time.Time) ([]byte, string, error) {
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	list := func(as []*netmail.Address) string {
		s := make([]string, len(as))
		for i, a := range as {
			s[i] = a.String()
		}
		return strings.Join(s, ", ")
	}
	id, err := messageID(from.Address)
	if err != nil {
		return nil, "", err
	}
	h("From", from.String())
	h("Reply-To", replyTo)
	if len(to) > 0 {
		h("To", list(to))
	}
	if len(cc) > 0 {
		h("Cc", list(cc))
	}
	h("Subject", mime.QEncoding.Encode("utf-8", headerSafe(o.Subject)))
	h("Date", now.Format(time.RFC1123Z))
	h("Message-ID", "<"+id+">")
	if inReplyTo != "" {
		h("In-Reply-To", "<"+inReplyTo+">")
		h("References", "<"+inReplyTo+">")
	}
	h("MIME-Version", "1.0")
	part := func(w *bytes.Buffer, body string) error {
		q := quotedprintable.NewWriter(w)
		if _, err := q.Write([]byte(body)); err != nil {
			return err
		}
		return q.Close()
	}
	switch {
	case o.Text != "" && o.HTML != "":
		mw := multipart.NewWriter(&b)
		h("Content-Type", `multipart/alternative; boundary="`+mw.Boundary()+`"`)
		b.WriteString("\r\n")
		for _, p := range []struct{ ct, body string }{{"text/plain", o.Text}, {"text/html", o.HTML}} {
			w, err := mw.CreatePart(textproto.MIMEHeader{
				"Content-Type":              {p.ct + "; charset=utf-8"},
				"Content-Transfer-Encoding": {"quoted-printable"},
			})
			if err != nil {
				return nil, "", err
			}
			var pb bytes.Buffer
			if err := part(&pb, p.body); err != nil {
				return nil, "", err
			}
			if _, err := w.Write(pb.Bytes()); err != nil {
				return nil, "", err
			}
		}
		if err := mw.Close(); err != nil {
			return nil, "", err
		}
	default:
		ct, body := "text/plain", o.Text
		if o.Text == "" {
			ct, body = "text/html", o.HTML
		}
		h("Content-Type", ct+"; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := part(&b, body); err != nil {
			return nil, "", err
		}
	}
	return b.Bytes(), id, nil
}

func messageID(from string) (string, error) {
	r := make([]byte, 12)
	if _, err := rand.Read(r); err != nil {
		return "", err
	}
	_, domain, _ := strings.Cut(from, "@")
	return "outpost." + hex.EncodeToString(r) + "@" + domain, nil
}

// smtpSend delivers over SMTP with STARTTLS, as Gmail's port 587 takes it.
// Logging in with PLAIN is refused without TLS, so a server that doesn't
// offer it fails rather than being sent the password in the clear.
func smtpSend(ctx context.Context, addr, user, pass, from string, to []string, msg []byte) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	d := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }() // Quit has closed it on success
	deadline := time.Now().Add(2 * time.Minute)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	_ = conn.SetDeadline(deadline) // a failed deadline only loses the bound
	c, err := smtp.NewClient(conn, host)
	if err != nil {
		return fmt.Errorf("talk to %s: %w", addr, err)
	}
	if ok, _ := c.Extension("STARTTLS"); !ok {
		return fmt.Errorf("%s doesn't offer STARTTLS, so the password isn't sent", addr)
	}
	if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
		return fmt.Errorf("start TLS with %s: %w", addr, err)
	}
	if err := c.Auth(smtp.PlainAuth("", user, pass, host)); err != nil {
		return loginError(err)
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range to {
		if err := c.Rcpt(r); err != nil {
			return fmt.Errorf("recipient %s: %w", r, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

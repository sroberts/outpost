package mail

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	netmail "net/mail"
	"strings"
	"time"

	_ "github.com/emersion/go-message/charset" // decode the charsets newsletters come in, not only UTF-8
	gomail "github.com/emersion/go-message/mail"
)

// maxPart bounds the text or HTML kept from one message. A newsletter's
// HTML is rarely past a few hundred kilobytes.
const maxPart = 2 << 20

// Addr is a name and address.
type Addr struct {
	Name    string `json:"name,omitempty"`
	Address string `json:"address"`
}

// Message is one message routed to an app.
type Message struct {
	ID int64 `json:"id"`
	// Route is the plus part it was sent to, and To that whole address.
	Route   string `json:"route"`
	To      string `json:"to"`
	From    Addr   `json:"from"`
	ReplyTo string `json:"reply_to,omitempty"`
	Subject string `json:"subject"`
	// MessageID is its Message-ID header, without the angle brackets.
	MessageID string    `json:"message_id,omitempty"`
	Date      time.Time `json:"date"`
	Received  time.Time `json:"received"`
	Text      string    `json:"text,omitempty"`
	HTML      string    `json:"html,omitempty"`
	// Attachments counts the parts that weren't kept: files, images.
	Attachments int `json:"attachments,omitempty"`

	service string
}

// recipients are the addresses a message was delivered to, from its header
// alone: Delivered-To first, since Gmail sets it to the address that
// actually got it, including a Bcc.
func recipients(header []byte) []string {
	m, err := netmail.ReadMessage(bytes.NewReader(append(bytes.TrimRight(header, "\r\n"), "\r\n\r\n"...)))
	if err != nil {
		return nil
	}
	var out []string
	for _, key := range []string{"Delivered-To", "X-Original-To"} {
		for _, v := range m.Header[key] {
			out = append(out, strings.Trim(strings.TrimSpace(v), "<>"))
		}
	}
	for _, key := range []string{"To", "Cc"} {
		for _, v := range m.Header[key] {
			list, err := (&netmail.AddressParser{WordDecoder: &mime.WordDecoder{}}).ParseList(v)
			if err != nil {
				continue
			}
			for _, a := range list {
				out = append(out, a.Address)
			}
		}
	}
	return out
}

// routingRecipients are the addresses a message is routed by. When the
// server recorded where it actually delivered the message, only that counts:
// To and Cc are written by the sender, so a forged "To: you+yarr@…" on mail
// delivered to you@… must not route it, nor must an app's address Cc'd on a
// thread sent to you. Without Delivered-To, the headers are all there is.
func routingRecipients(header []byte) []string {
	all := recipients(header)
	m, err := netmail.ReadMessage(bytes.NewReader(append(bytes.TrimRight(header, "\r\n"), "\r\n\r\n"...)))
	if err != nil {
		return all
	}
	n := len(m.Header["Delivered-To"]) + len(m.Header["X-Original-To"])
	if n == 0 {
		return all
	}
	return all[:n]
}

// parse reads a whole message: its headers and the first plain text and
// HTML parts. Attachments are counted, not kept.
func parse(raw []byte) (*Message, error) {
	r, err := gomail.CreateReader(bytes.NewReader(raw))
	if err != nil && r == nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	defer func() { _ = r.Close() }() // read from memory
	m := &Message{}
	h := r.Header
	m.Subject, _ = h.Subject()
	if m.Subject == "" {
		m.Subject = h.Get("Subject")
	}
	if from, err := h.AddressList("From"); err == nil && len(from) > 0 {
		m.From = Addr{Name: from[0].Name, Address: from[0].Address}
	} else {
		m.From = Addr{Address: h.Get("From")}
	}
	if rt, err := h.AddressList("Reply-To"); err == nil && len(rt) > 0 {
		m.ReplyTo = rt[0].Address
	}
	m.MessageID, _ = h.MessageID()
	if d, err := h.Date(); err == nil {
		m.Date = d.UTC()
	}
	for {
		p, err := r.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A malformed part late in a message still leaves what came
			// before it worth keeping.
			if m.Text == "" && m.HTML == "" {
				return nil, fmt.Errorf("read message body: %w", err)
			}
			break
		}
		switch ph := p.Header.(type) {
		case *gomail.InlineHeader:
			ct, _, _ := ph.ContentType()
			body, err := io.ReadAll(io.LimitReader(p.Body, maxPart))
			if err != nil {
				continue
			}
			switch {
			case ct == "text/plain" && m.Text == "":
				m.Text = string(body)
			case ct == "text/html" && m.HTML == "":
				m.HTML = string(body)
			case ct != "text/plain" && ct != "text/html":
				m.Attachments++
			}
		case *gomail.AttachmentHeader:
			m.Attachments++
		}
	}
	return m, nil
}

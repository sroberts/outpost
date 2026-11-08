package config

import (
	"fmt"
	"net"
	"net/mail"
	"strings"
	"time"
)

// Defaults for [mail].
const (
	DefaultMailListen   = "127.0.0.1:47902"
	DefaultMailPoll     = time.Minute
	DefaultMailIMAP     = "imap.gmail.com:993"
	DefaultMailSMTP     = "smtp.gmail.com:587"
	DefaultMailPassword = "gmail-app-password"
)

// Mail is the Gmail account apps receive and send mail through, each at its
// own plus address. The portal's Mail page can set the account too, and its
// choice wins. See docs/design/mail.md.
type Mail struct {
	// Listen is the loopback address of the endpoint apps call, which they
	// are told as OUTPOST_MAIL_URL.
	Listen string `toml:"listen"`
	// Address is the Gmail account, such as you@gmail.com. Unset means none
	// until the portal sets one.
	Address string `toml:"address"`
	// Poll is how often the inbox is checked, such as "1m".
	Poll string `toml:"poll"`
	// IMAP and SMTP are the servers, as host:port; unset is Gmail's.
	IMAP string `toml:"imap"`
	SMTP string `toml:"smtp"`
	// PasswordSecret is the secret the account's app password is kept in;
	// unset is "gmail-app-password".
	PasswordSecret string `toml:"password_secret"`
}

// ListenAddr is Listen, or the default.
func (m Mail) ListenAddr() string {
	if m.Listen == "" {
		return DefaultMailListen
	}
	return m.Listen
}

// PollEvery is Poll, or the default. An unparsable one is refused by
// validate, so it can't reach here from a loaded file.
func (m Mail) PollEvery() time.Duration {
	d, err := time.ParseDuration(m.Poll)
	if err != nil || d <= 0 {
		return DefaultMailPoll
	}
	return d
}

// IMAPAddr, SMTPAddr and PasswordName are their settings, or the defaults.
func (m Mail) IMAPAddr() string     { return orDefault(m.IMAP, DefaultMailIMAP) }
func (m Mail) SMTPAddr() string     { return orDefault(m.SMTP, DefaultMailSMTP) }
func (m Mail) PasswordName() string { return orDefault(m.PasswordSecret, DefaultMailPassword) }

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func (m Mail) validate() error {
	host, port, err := net.SplitHostPort(m.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen %q: want host:port, like %s", m.Listen, DefaultMailListen)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen %q: the mail endpoint is for apps on this machine, so it has to be a loopback address, like %s", m.Listen, DefaultMailListen)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("listen %q: give a fixed port; apps keep the address across daemon restarts", m.Listen)
	}
	if m.Address != "" {
		a, err := mail.ParseAddress(m.Address)
		if err != nil || a.Name != "" || a.Address != m.Address || strings.Contains(strings.SplitN(a.Address, "@", 2)[0], "+") {
			return fmt.Errorf("address %q: want the account's own address, like you@gmail.com, with no + part", m.Address)
		}
	}
	if m.Poll != "" {
		d, err := time.ParseDuration(m.Poll)
		if err != nil || d < 10*time.Second {
			return fmt.Errorf("poll %q: want a duration of at least 10s, like \"1m\"", m.Poll)
		}
	}
	for key, v := range map[string]string{"imap": m.IMAP, "smtp": m.SMTP} {
		if v == "" {
			continue
		}
		if h, p, err := net.SplitHostPort(v); err != nil || h == "" || p == "" {
			return fmt.Errorf("%s %q: want host:port", key, v)
		}
	}
	if m.PasswordSecret != "" && !secretName.MatchString(m.PasswordSecret) {
		return fmt.Errorf("password_secret %q: a secret's name is 1-64 characters of a-z, 0-9, '.', '_' and '-'", m.PasswordSecret)
	}
	return nil
}

func (m Mail) render(b *strings.Builder) {
	b.WriteString("[mail]\n")
	b.WriteString("# The Gmail account apps get mail through, each at its own plus\n")
	b.WriteString("# address, such as you+yarr@gmail.com. Every app gets OUTPOST_MAIL_URL\n")
	b.WriteString("# and its own OUTPOST_MAIL_TOKEN. The app password is the secret named\n")
	b.WriteString("# by password_secret. The portal's Mail page can set both, and wins.\n")
	optional := func(key, val, example string) {
		if val != "" {
			fmt.Fprintf(b, "%s = %q\n", key, val)
		} else {
			fmt.Fprintf(b, "# %s = %q\n", key, example)
		}
	}
	optional("address", m.Address, "you@gmail.com")
	optional("password_secret", m.PasswordSecret, DefaultMailPassword)
	b.WriteString("# How often the inbox is checked.\n")
	optional("poll", m.Poll, "1m")
	optional("imap", m.IMAP, DefaultMailIMAP)
	optional("smtp", m.SMTP, DefaultMailSMTP)
	b.WriteString("# The endpoint apps call; loopback only.\n")
	optional("listen", m.Listen, DefaultMailListen)
}

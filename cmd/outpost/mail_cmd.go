package main

import (
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/mail"
)

// mailOptions is config.toml's [mail] as the mail service takes it. The app
// password is kept in secrets.db, like the AI providers' keys.
func mailOptions(c config.Mail, path string, keys mail.Keys, logf func(string, ...any)) mail.Options {
	return mail.Options{
		Path:           path,
		Listen:         c.ListenAddr(),
		Address:        c.Address,
		Poll:           c.PollEvery(),
		IMAP:           c.IMAPAddr(),
		SMTP:           c.SMTPAddr(),
		Keys:           keys,
		PasswordSecret: c.PasswordName(),
		Logf:           logf,
	}
}

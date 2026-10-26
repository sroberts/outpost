package auth

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Usernames are what apps see in X-Outpost-User, so they must be safe as a
// header value, a path segment and a database key without escaping.
var usernameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

const nameRule = "1–32 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit"

// ValidUsername reports whether s can be used as a username.
func ValidUsername(s string) error {
	if !usernameRE.MatchString(s) {
		return fmt.Errorf("invalid username %q: use %s", s, nameRule)
	}
	return nil
}

// ValidGroup reports whether s can be used as a group name. Groups follow the
// username rule because they travel the same way, in X-Outpost-Groups.
func ValidGroup(s string) error {
	if !usernameRE.MatchString(s) {
		return fmt.Errorf("invalid group %q: use %s", s, nameRule)
	}
	return nil
}

// MinPasswordLen is the only rule a password has to meet. Length is what
// makes a password hard to guess; composition rules mostly make it hard to
// remember.
const MinPasswordLen = 12

// maxPasswordLen bounds the work one login can ask argon2 to do.
const maxPasswordLen = 1024

// ValidPassword checks a new password's length, counted in characters.
func ValidPassword(p string) error {
	n := utf8.RuneCountInString(p)
	if n < MinPasswordLen {
		return fmt.Errorf("password is too short: use at least %d characters", MinPasswordLen)
	}
	if len(p) > maxPasswordLen {
		return fmt.Errorf("password is too long: use at most %d bytes", maxPasswordLen)
	}
	return nil
}

// maxTextLen bounds the free-text fields.
const maxTextLen = 200

// validText checks a display name or email. Both end up in HTTP headers, so
// a control character here would let a stored value split a header.
func validText(field, s string) error {
	if len(s) > maxTextLen {
		return fmt.Errorf("%s is too long: use at most %d bytes", field, maxTextLen)
	}
	if !utf8.ValidString(s) {
		return fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

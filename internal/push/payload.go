package push

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Limits on what a notification says. A lock screen shows a line or two, so
// these are generous for that and keep a payload well inside MaxPayload.
const (
	maxTitle = 120
	maxBody  = 1000
	maxTag   = 64
	maxPath  = 1024
)

// Urgency values (RFC 8030 §5.3).
var urgencies = map[string]bool{"very-low": true, "low": true, "normal": true, "high": true}

// Notification is one message for people's devices.
type Notification struct {
	// Title is required; Body is the line under it.
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
	// Path is where on the sender tapping it goes: a path on the app's own
	// host for an app, on the portal for Outpost's own. Unset is "/".
	Path string `json:"path,omitempty"`
	// Tag replaces an earlier notification with the same tag on a device,
	// rather than stacking another.
	Tag string `json:"tag,omitempty"`
	// Badge, when set, is the number on the Home Screen or Dock icon; 0
	// clears it.
	Badge *int `json:"badge,omitempty"`
	// Silent asks the device not to make a sound.
	Silent bool `json:"silent,omitempty"`
	// Urgency is very-low, low, normal (the default) or high: how far a
	// device on low battery goes to deliver it.
	Urgency string `json:"urgency,omitempty"`
	// TTL is how long, in seconds, the push service holds it for a device
	// that is off; 0 is the default, a day.
	TTL int `json:"ttl,omitempty"`
	// Topic replaces one still waiting at the push service with the same
	// topic, so a device that was off gets only the latest.
	Topic string `json:"topic,omitempty"`
}

// DefaultTTL is how long a push service holds a notification for a device
// that's off, when the sender doesn't say.
const DefaultTTL = 24 * 60 * 60

// maxTTL is four weeks, past which push services cap it anyway.
const maxTTL = 28 * 24 * 60 * 60

// badRequest is a notification that can't be sent as it stands; its text
// is for whoever wrote it.
type badRequest struct{ msg string }

func (e badRequest) Error() string { return e.msg }

func bad(format string, a ...any) error { return badRequest{fmt.Sprintf(format, a...)} }

// IsBadRequest reports whether err is a fault in the notification itself.
func IsBadRequest(err error) bool {
	var br badRequest
	return errors.As(err, &br)
}

// check tidies n and says what's wrong with it, if anything.
func (n *Notification) check() error {
	n.Title = strings.TrimSpace(n.Title)
	n.Body = strings.TrimSpace(n.Body)
	switch {
	case n.Title == "":
		return bad("a title is required")
	case utf8.RuneCountInString(n.Title) > maxTitle:
		return bad("the title is at most %d characters", maxTitle)
	case utf8.RuneCountInString(n.Body) > maxBody:
		return bad("the body is at most %d characters", maxBody)
	case len(n.Tag) > maxTag:
		return bad("the tag is at most %d characters", maxTag)
	}
	if n.Path == "" {
		n.Path = "/"
	}
	// A path on the sender's own host and nothing else: no scheme, no
	// other host, and not //host, which a browser reads as one.
	if len(n.Path) > maxPath || !strings.HasPrefix(n.Path, "/") || strings.HasPrefix(n.Path, "//") ||
		strings.ContainsAny(n.Path, "\\\r\n\t") {
		return bad("path is a path on your own host, starting with one /, like /tasks/42")
	}
	if n.Badge != nil && *n.Badge < 0 {
		return bad("badge can't be negative")
	}
	if n.Urgency == "" {
		n.Urgency = "normal"
	}
	if !urgencies[n.Urgency] {
		return bad("urgency is very-low, low, normal or high")
	}
	switch {
	case n.TTL < 0:
		return bad("ttl can't be negative")
	case n.TTL == 0:
		n.TTL = DefaultTTL
	case n.TTL > maxTTL:
		n.TTL = maxTTL
	}
	// RFC 8030 §5.4: at most 32 characters of the base64url alphabet.
	if len(n.Topic) > 32 || strings.IndexFunc(n.Topic, notBase64URL) >= 0 {
		return bad("topic is at most 32 letters, digits, '-' and '_'")
	}
	return nil
}

// notBase64URL reports a rune outside base64url's alphabet.
func notBase64URL(r rune) bool {
	inAlphabet := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
	return !inAlphabet
}

// declarative is a Declarative Web Push message: Safari shows it without
// running any script, and the portal's service worker shows the same JSON
// everywhere else. "web_push": 8030 is what marks it as one.
type declarative struct {
	WebPush      int          `json:"web_push"`
	Notification declarNotice `json:"notification"`
	AppBadge     string       `json:"app_badge,omitempty"`
	Mutable      bool         `json:"mutable"`
}

type declarNotice struct {
	Title    string         `json:"title"`
	Body     string         `json:"body,omitempty"`
	Navigate string         `json:"navigate"`
	Lang     string         `json:"lang,omitempty"`
	Dir      string         `json:"dir,omitempty"`
	Tag      string         `json:"tag,omitempty"`
	Silent   bool           `json:"silent"`
	Data     map[string]any `json:"data,omitempty"`
}

// payload is n as the JSON a device is sent, opening navigate when tapped.
// Mutable stays false: nothing Outpost sends needs a script to change it on
// the device, and Safari shows a declarative message more reliably than one
// a worker has to handle.
func payload(n Notification, navigate, from string) ([]byte, error) {
	d := declarative{
		WebPush: 8030,
		Notification: declarNotice{
			Title:    n.Title,
			Body:     n.Body,
			Navigate: navigate,
			Dir:      "auto",
			Tag:      n.Tag,
			Silent:   n.Silent,
			Data:     map[string]any{"from": from},
		},
	}
	if n.Badge != nil {
		d.AppBadge = fmt.Sprint(*n.Badge)
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxPayload {
		return nil, bad("the notification comes to %d bytes, and at most %d fit; shorten the body", len(b), MaxPayload)
	}
	return b, nil
}

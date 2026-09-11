package tsnet

import (
	"strings"
	"time"
)

// loginReminderInterval is how often an unauthorised node's login URL is worth
// repeating in the log.
//
// tsnet reprints it every five seconds for as long as the node sits in
// NeedsLogin, which is how one unauthorised service produced 54 identical
// lines. A node nobody has authorised is still worth an occasional reminder to
// whoever reads the log days later; 720 lines a day is not.
const loginReminderInterval = 15 * time.Minute

// parseLoginURL pulls the authorisation URL out of a line tsnet handed to
// UserLogf, already formatted.
//
// It matches on shape rather than on tsnet's exact wording, which has changed
// between releases: a login URL that stops being recognised is a node nobody
// is told how to authorise. A URL on its own is not enough, though — tsnet logs
// other lines carrying one — so the rest of the line has to read as an auth
// prompt too.
func parseLoginURL(line string) (string, bool) {
	var url string
	for _, field := range strings.Fields(line) {
		if strings.HasPrefix(field, "https://") {
			url = strings.TrimRight(field, `.,;:!?)]}>"'`)
			break
		}
	}
	if url == "" || url == "https://" {
		return "", false
	}

	// The hints are looked for everywhere but the URL itself, because a login
	// URL contains "login" whatever the line around it says.
	rest := strings.ToLower(strings.Replace(line, url, " ", 1))
	for _, hint := range []string{"ts_authkey", "authenticat", "log in", "login"} {
		if strings.Contains(rest, hint) {
			return url, true
		}
	}
	return "", false
}

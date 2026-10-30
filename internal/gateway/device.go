package gateway

import "strings"

// DescribeAgent names the browser and system a User-Agent claims, in a few
// words, for telling sign-ins apart: "Firefox on Windows". It reads only
// the common shapes and says so when it can't; the header is the client's
// to choose, so this is a label, never evidence.
func DescribeAgent(ua string) string {
	if ua == "" {
		return "Unknown browser"
	}
	browser := ""
	// Order matters: Edge and Opera claim Chrome, Chrome claims Safari, and
	// iOS browsers other than Safari name themselves with their own token.
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"EdgiOS/", "Edge"}, {"OPR/", "Opera"},
		{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"},
		{"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
		{"Safari/", "Safari"},
		{"curl/", "curl"}, {"Wget/", "Wget"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	system := ""
	for _, s := range []struct{ token, name string }{
		{"iPhone", "iPhone"}, {"iPad", "iPad"}, {"Android", "Android"},
		{"CrOS", "ChromeOS"}, {"Mac OS X", "Mac"}, {"Macintosh", "Mac"},
		{"Windows", "Windows"}, {"Linux", "Linux"},
	} {
		if strings.Contains(ua, s.token) {
			system = s.name
			break
		}
	}
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case browser != "":
		return browser
	case system != "":
		return "A browser on " + system
	}
	return "Unknown browser"
}

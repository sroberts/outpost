package runtime

import "strings"

// daemonSecrets are credentials the daemon itself may be given, which no
// child of it needs: a service, possibly a third-party binary from
// `outpost add <url>`, must not be able to join nodes to the tailnet or act
// on GitHub as the operator. A tsnet node gets its key passed explicitly.
var daemonSecrets = map[string]bool{
	"TS_AUTHKEY":            true,
	"TS_AUTH_KEY":           true,
	"GITHUB_TOKEN":          true,
	"GH_TOKEN":              true,
	"OUTPOST_TSNET_AUTHKEY": true,
}

// WithoutSecrets returns env, in os.Environ form, minus the daemon's own
// credentials, for building a child process's environment.
func WithoutSecrets(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if daemonSecrets[k] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

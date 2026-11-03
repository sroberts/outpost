package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// daemonAdds lets the gateway do what `outpost add <repo>` does, by running
// it, so a service added from the portal is looked up, checked, installed and
// declared by exactly the same code as one added from a terminal.
type daemonAdds struct{ a *app }

// Preview is `outpost add --print <repo>`: the manifest it would write and the
// asset it would install, with nothing downloaded or written. The add command
// bounds its own requests, so ctx is only checked before starting.
func (d daemonAdds) Preview(ctx context.Context, repo string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return d.run("--print", repo)
}

// Add is `outpost add <repo>`. Nothing else can be asked for: no --force, so
// the portal can never replace a service that's already declared, and no
// flags, so what's installed is what the release's descriptor says.
func (d daemonAdds) Add(repo string) (string, error) {
	return d.run(repo)
}

func (d daemonAdds) run(args ...string) (string, error) {
	repo := args[len(args)-1]
	// The gateway passes owner/name; anything else here is a bug, and one
	// starting with a dash would be read as a flag.
	if strings.HasPrefix(repo, "-") || !isRepoRef(repo) {
		return "", fmt.Errorf("not a repository: %q", repo)
	}
	var out bytes.Buffer
	run := *d.a
	run.out, run.err = &out, &out
	err := run.cmdAdd(args)
	return out.String(), err
}

# Security policy

Outpost's login gateway handles passwords, sessions and access tokens, and its
daemon runs services on your machine, so reports are welcome and taken
seriously.

## Supported versions

Outpost has no stable release yet. Fixes go to `main` and into the next
release; only the latest release receives security fixes.

## Reporting a vulnerability

Please **don't** open a public issue for security problems.

Report it privately through [GitHub's private vulnerability reporting](https://github.com/sroberts/outpost/security/advisories/new).

Include what you found, how to reproduce it, and the impact you expect: for
example, which of the gateway, the control socket, the tailnet nodes or a
manifest it involves, and whether it needs local access. You can expect an
acknowledgement within TODO(maintainer): a response time, such as 7 days, and
updates until it's fixed and disclosed.

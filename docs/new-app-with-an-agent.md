# Starting a new app with a coding agent

How to start an app that runs under your Outpost when a coding agent (Claude
Code, Codex, Cursor and the like) is writing it. The idea is that the agent
learns the rules from your running server, not from memory or a stale copy, and
can check back as Outpost improves.

This assumes the login gateway is on, so the portal serves the contract. If it
isn't, use `outpost spec` on the server in step 2 instead of `curl`.

## The short way: hand this to your agent

Two things have to come from you. First, make a token on the server (step 1)
and put it in the agent's environment as `OUTPOST_TOKEN`. It goes there, not
in the prompt, so it stays out of chat logs and the repository. Then fill in the
two placeholders below and paste the whole block to the agent in an empty
directory. It does steps 2 to 4 itself.

````text
You're starting a new app that will run under Outpost, a supervisor for local
services. Outpost publishes the contract an app must meet, and that contract
wins over anything you already believe about Outpost.

App: <app name>, which <does what, in a sentence>.
Outpost: <portal URL, e.g. https://outpost.example>
OUTPOST_TOKEN is set in your environment. Never print it, write it to a file,
or commit it.

1. Set up the repository and fetch what Outpost supports:

   git init
   curl -fsS "<portal URL>/.well-known/outpost-service-spec.md" -o outpost-service-spec.md
   curl -fsS -H "Authorization: Bearer $OUTPOST_TOKEN" \
     "<portal URL>/.well-known/outpost.json" -o .outpost.json

   If either request fails, stop and tell me why. A 401 means the token is
   wrong or expired. Don't carry on from memory.

2. Read outpost-service-spec.md in full. .outpost.json says which Outpost
   version and contract revision you are building against, and lists the
   features with the section that covers each.

3. Write a "Running under Outpost" section into CLAUDE.md (or AGENTS.md if
   that's what this repository uses) so later sessions follow the contract
   without this prompt. It should say:
   - outpost-service-spec.md is the contract and wins over anything else;
     read it before changing how the app starts, listens, configures itself,
     stores data, shuts down or logs.
   - The rules this app depends on, in a few lines each, with their section
     numbers: the loopback port from OUTPOST_PORT, the /up healthcheck,
     state under OUTPOST_STORAGE_DIR, SIGTERM, logging, and how identity
     behind the gateway works and when to verify X-Outpost-Identity.
   - How to check for a newer contract: fetch
     <portal URL>/.well-known/outpost.json with the OUTPOST_TOKEN bearer token,
     and if contract.revision differs from the one in .outpost.json, fetch the
     spec again, diff it against the committed copy, report which changes
     matter to this app before changing code, then commit the new spec and
     .outpost.json together.

4. Build the app to the contract. Where .outpost.json lists a feature that
   fits, such as identity from the gateway instead of logins of its own, use
   it rather than building your own.

5. Verify against a local build, without Outpost: start it with OUTPOST_PORT
   and OUTPOST_STORAGE_DIR set, walk the §11 conformance checklist, and run the
   §13 checks. Fix whatever fails.

6. Commit, and report: the contract revision you built against, the features
   you used, any checklist item you couldn't meet and why, and the exact
   `outpost add` command to run it on the server.
````

When it's done, deploy as in step 5 below. The CLAUDE.md section the agent wrote
covers checking back after an upgrade (step 6).

The rest of this guide is the same setup done by hand, for when you want to
control each step.

## 1. Give the agent a token

On the machine running Outpost, make an access token for yourself and limit it
to the app you're about to build. The app doesn't need to exist yet: the token
can reach it once it does, and nothing else.

```sh
outpost token create <you> --name <app>-agent --app <app> --expires 90d
```

`outpost.json` takes any of your tokens, whatever it's scoped to. A token
limited to the new app lets the agent read what Outpost supports and later
call the app through the gateway, and reach nothing else of yours. Keep it out
of the repository: put it in your shell or the agent's environment as
`OUTPOST_TOKEN`, alongside `OUTPOST_URL` (your portal, for example
`https://outpost.example`).

## 2. Put the contract in the new repository

```sh
mkdir <app> && cd <app> && git init
curl -fsS "$OUTPOST_URL/.well-known/outpost-service-spec.md" -o outpost-service-spec.md
curl -fsS -H "Authorization: Bearer $OUTPOST_TOKEN" \
  "$OUTPOST_URL/.well-known/outpost.json" -o .outpost.json
```

- `outpost-service-spec.md` is the contract the app must meet: one loopback
  port, a healthcheck, configuration from the environment, state in one
  directory, SIGTERM, logs to stdout, and the optional parts (identity behind
  the gateway, secrets, AI models, releases `outpost add` can install).
- `.outpost.json` records which Outpost you built against: its version, the
  contract's `revision`, the identity issuer and JWKS URL, and the features
  with the section that describes each.

Commit both. The committed revision is how you'll know later whether the
contract has moved on.

## 3. Tell the agent how to use them

Add this to the repository's agent instructions (`CLAUDE.md`, `AGENTS.md`, or
whatever your agent reads), filling in the name and port:

````markdown
## Running under Outpost

This app is supervised by Outpost. `outpost-service-spec.md` is the contract it
must satisfy and wins over anything else you know about Outpost. Read it before
changing how the app starts, listens, configures itself, stores data, shuts
down or logs. `.outpost.json` records the Outpost we last built against.

- Listen on `127.0.0.1` only, in plain HTTP, on the port in `OUTPOST_PORT`
  (flag `--port` wins over it). Local default: <port>.
- Serve `GET /up`: 200 when ready, non-2xx before, no auth.
- Keep all state under `OUTPOST_STORAGE_DIR`.
- Behind the gateway (`OUTPOST_AUTH=gateway`), `X-Outpost-User` and friends
  say who is calling. For anything another process on the machine must not do
  as someone else, verify `X-Outpost-Identity` against `OUTPOST_JWKS_URL`
  (§3). With `OUTPOST_AUTH` unset, trust none of these headers.
- Check `Origin` on anything that changes state: every app behind the gateway
  shares one site (§3).
- Before finishing, walk the conformance checklist (§11) and the checks in §13.

### Checking for a newer contract

`OUTPOST_URL` and `OUTPOST_TOKEN` are in the environment. When asked to check
Outpost, or before work that touches the contract:

```sh
curl -fsS -H "Authorization: Bearer $OUTPOST_TOKEN" "$OUTPOST_URL/.well-known/outpost.json"
```

If `contract.revision` differs from the one in `.outpost.json`, fetch
`$OUTPOST_URL/.well-known/outpost-service-spec.md`, diff it against the
committed copy, and say which changes matter to this app before changing
code. Then commit the new spec and `.outpost.json` together.
````

## 4. Start the agent

Something like:

> Build `<app>`, a service that <does what>. It runs under Outpost: read
> `CLAUDE.md` and `outpost-service-spec.md` first. Use the features in
> `.outpost.json` where they help, such as identity from the gateway rather
> than logins of its own. Finish with the §11 checklist, and the §13 checks run
> against a local build.

The agent can do the §13 checks on its own machine without Outpost: start the
binary with `OUTPOST_PORT` and `OUTPOST_STORAGE_DIR` set and probe it.

## 5. Run it under Outpost

Build for the server's platform and add it by path:

```sh
GOOS=linux GOARCH=arm64 go build -o <app> .   # or however the app builds
scp <app> server:/opt/outpost/bin/
outpost add <app> --binary /opt/outpost/bin/<app> --port <port>
```

`outpost add` writes `OUTPOST_PORT` into the manifest it generates, so an app
that reads it needs no `--args`. Once it's running, the agent's token reaches it
at the app's address like any other client. To let `outpost add <github-url>`
install it and `outpost update` keep it current, publish releases as §14
describes.

## 6. When Outpost improves

After upgrading Outpost, ask the agent to check for a newer contract. The step 3
instructions tell it what to do. Or check by hand:

```sh
jq -r .contract.revision .outpost.json
curl -fsS -H "Authorization: Bearer $OUTPOST_TOKEN" "$OUTPOST_URL/.well-known/outpost.json" | jq -r .contract.revision
```

The same value means nothing has changed. A different one means read the new
spec. The spec's `ETag` is the same revision, so a script can ask with
`If-None-Match` and get a `304` when there's nothing new. When the token
expires, make another with step 1.

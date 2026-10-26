# Contributing

Welcome. This guide is the canonical source for how PRs get reviewed
and merged in this repository — both for human contributors and for
AI coding agents.

## Workflow

1. Branch from main: `git checkout -b <type>/<short-name>`.
2. Make focused commits — one logical change per PR.
3. Run `make check` before opening the PR. It's exactly what CI runs: format,
   vet, lint, the test suite and the race detector.
4. Open a PR; the template lists the per-merge checks.

## What gets a PR rejected

- CI not green: tests, the race detector, `gofmt`, a tidy `go.mod`, or
  `golangci-lint` (errcheck included).
- Existing tests edited or deleted without the owner's approval. Adding tests
  is welcome; if a change breaks an existing test, rethink the change.
- Anything that needs cgo. The binary must stay statically linked, which is
  why the SQLite driver is `modernc.org/sqlite`.
- Logic that treats `state.db` as authoritative. The manifests are the source
  of truth and `state.db` is a cache of what was observed.
- New supervisor API that touches its state directly instead of going through
  `submit()` and the reconcile loop.
- Edits to `spec.md`, which belongs to the repository owner.
- More than one logical change, or reformatting of lines the change doesn't
  touch.

## Style

- Format with the language's standard tool (gofmt, prettier, etc.).
- Don't reformat unrelated lines; keep diffs minimal.
- Comments explain WHY, not WHAT.

## Asking for help

Open a draft PR or an issue. Don't sit on a stuck branch.

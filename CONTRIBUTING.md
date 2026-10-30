# Contributing

Welcome. This guide is the canonical source for how PRs get reviewed
and merged in this repository — both for human contributors and for
AI coding agents.

## Setting up

- Go 1.26.6 or newer, as `go.mod` says. Nothing else is needed to build: no
  cgo, no C toolchain.
- [golangci-lint](https://golangci-lint.run/) v2.11.4, the version CI runs,
  for `make lint` and `make check`. If it fails to run against your Go
  version, `go run honnef.co/go/tools/cmd/staticcheck@latest ./...` covers
  most of the same ground until it catches up; CI is the final word.
- `make help` lists every target.

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

## Releasing

Pushing a `v*` tag from `main` runs `.github/workflows/release.yml`, which
cross-compiles static binaries for darwin and linux on arm64 and amd64, stamps
the tag into them, and publishes a GitHub release with generated notes and a
`SHA256SUMS` file. Versions follow [semantic versioning](https://semver.org/);
before 1.0, a minor bump may break the manifest or config format, and the
release notes say so.

```sh
git checkout main && git pull
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

## Style

- Format with `make fmt`, never `gofmt -w` on a directory:
  `internal/manifest/manifest_test.go` is deliberately left unformatted.
- Don't reformat unrelated lines; keep diffs minimal.
- Comments explain WHY, not WHAT.

## Asking for help

Open a draft PR or an issue. Don't sit on a stuck branch.

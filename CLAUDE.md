# Claude working notes for packyard

Context and conventions for future Claude sessions working in this
repo. Not a user-facing doc — [README.md](README.md) and
[docs/](docs/) are for that.

## What this is

A single-binary Go server that hosts internal R packages over a
CRAN-protocol-compatible HTTP surface. SQLite + local filesystem by
default; no external services. Operators publish via CI (multipart
POST), R clients consume via unchanged `install.packages()`.
[design.md](design.md) has the full architectural picture,
[implementation.md](implementation.md) the phased build plan.

## Stability policy — v1.x

**Adoption is effectively zero right now.** Breaking changes between
v1.x releases are fine as long as the release notes call them out.
Do **not** add backwards-compat shims, deprecation wrappers, dual
code paths, or renamed-but-kept aliases unless we have real users
who'd break. Fix the surface, rev the patch, move on.

If adoption picks up later, this policy flips and we hold API
contracts until 2.0. Until then: prefer clean over compatible.

## Commands

```sh
make build         # build ./packyard-server
make test          # go test -race ./...
make check         # vet + lint + test + openapi-lint
make lint          # golangci-lint v2
make fmt           # gofmt -s -w .
```

`make lint` installs `github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`
if it isn't on PATH. **Note the `/v2/` in the module path** — the
pre-v2 module path installs an incompatible v1.x release.

## Runtime invariants worth knowing before changing code

- **`-data` default is `./data`.** In the Docker image, `WORKDIR
  /data`; always pass `-data /data` explicitly when invoking the
  container — otherwise the resolved dir is `/data/data`.
- **Tokens are stored only as `sha256(token)`.** The plaintext is
  printed once at mint time. Losing the DB means every token has to
  be reissued; no recovery path.
- **CAS is content-addressed.** Writes go through a temp file and
  atomically rename into `cas/<aa>/<rest-of-sha256>`. Half-written
  blobs live under `cas/tmp/` and never leak into the
  content-addressed namespace.
- **`channels.yaml` reconcile is additive only.** On startup the
  server inserts channels present in YAML but missing from the DB,
  and warns on channels present in the DB but absent from YAML. It
  never deletes DB rows — that would silently orphan packages.
- **`BeginTx(ctx, nil)` currently starts a deferred tx.** Flagged
  for `BEGIN IMMEDIATE` in v1.1 (implementation.md §Post-v1 follow-ups).
- **Server auto-bootstraps a fresh data dir on `runServe`.** Missing
  `channels.yaml` / `matrix.yaml` are written from embedded defaults
  before the server starts listening. `-init` is only needed if you
  want to mint a token *before* the first serve.

## Release cutting

Tag push → GoReleaser → draft release + GHCR image. No manual steps
between commit and tagged artifact.

```sh
git tag -a v1.0.X -m "packyard v1.0.X"
git push origin v1.0.X
gh run watch    # ~2.5 min
# Review the draft at github.com/schochastics/packyard/releases
# Click Publish when ready.
```

**Tag convention:** Git tag is `vX.Y.Z`, GHCR image tag is `X.Y.Z`
(no `v` prefix). This is goreleaser's default `{{ .Version }}`
behaviour. `ghcr.io/schochastics/packyard:1.0.1` works;
`:v1.0.1` returns `manifest unknown`.

`:latest` is updated on every tag.

## Repo gotchas worth memorising

- **Direct push to `main` is blocked in this environment.** After
  committing, report the commit SHA and wait for the user to push.
- **GHCR packages default to private on first push.** Once a package
  is public it stays public; first release requires a one-time flip
  in the UI under `github.com/users/schochastics/packages/container/packyard/settings`.
- **`go 1.25.0` is pinned.** `modernc.org/sqlite v1.49.1` requires
  it. Do not bump the `go` directive downward.
- **IDE spelling warnings on `packyard`, `CRAN`, `organisation`, `renv`,
  `runbook` etc. are noise.** The IDE's spell checker flags
  project-specific vocabulary and British English; ignore them.
- **`[skip ci]` in a commit message suppresses tag-triggered Release
  runs too.** Learned the hard way on v1.1.0: a `[skip ci] draft
  off` commit landed on main, the v1.1.0 tag was created on that
  commit, and Release never fired even though the tag push itself
  was delivered. If you want to change a build/release config and
  then tag, either drop `[skip ci]` or add a follow-up commit
  without it before tagging.
- **Plan mode re-entry:** when re-entering plan mode, read the
  existing plan file first and overwrite if the new task is distinct
  (this is what system reminders already instruct).

## Test posture

- Target: 75%+ coverage per `internal/*` package. Current numbers
  (April 2026): api 81%, auth 93%, cas 82%, config 85%, db 82%,
  importers 70%, ui 80%.
- Integration-style tests spin up a real HTTP server via
  `httptest.NewServer` and exercise the on-the-wire surface. Prefer
  them for anything that crosses handler / DB / CAS boundaries.
- **No fuzz tests in v1.** Multipart publish is the obvious target;
  committed to v1.1.
- `internal/metrics` has 0% coverage by design — it's definitions
  plus registrations, exercised transitively by `metrics_*_test.go`
  in `internal/api`.
- CRAN-protocol compliance is HTTP/byte-level in
  [cran_protocol_test.go](internal/api/cran_protocol_test.go);
  we do **not** run `Rscript install.packages()` in CI today.
  Planned for v1.1.

## Memory vs this file

- `CLAUDE.md` (this file) — **repo-anchored** facts. Survives `rm -rf`
  of the machine; versioned with the code.
- `~/.claude/projects/-Users-david-projects-pakman/memory/` —
  **session-scoped** memory (user profile, preferences, project state
  between sessions, feedback). Machine-local; not committed.

Put stable repo facts here. Put ephemeral working state and anything
about the user in memory.

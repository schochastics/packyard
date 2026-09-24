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

- **`-data` default is `./data`.** The Docker images end with
  `WORKDIR /`, so inside the container it resolves to the `/data`
  volume. (Before v1.2.1 the workdir was `/data` and the default
  silently became `/data/data`.)
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
- **`BeginTx(ctx, nil)` starts `BEGIN IMMEDIATE`** (`_txlock=immediate`
  in the DSN, [internal/db/db.go](internal/db/db.go)). Every tx is a
  write; readers use plain queries.
- **`server.yaml` `tokens:` are reconciled on every start**
  (`auth.SyncConfigTokens`): insert, rescope, revoke-if-removed, for
  `tokens.source = 'config'` rows only. The API refuses to revoke
  config tokens.
- **Server auto-bootstraps a fresh data dir on `runServe`.** Missing
  `channels.yaml` / `matrix.yaml` are written from embedded defaults
  before the server starts listening. `-init` is only needed if you
  want to mint a token *before* the first serve.

## Release cutting

Tag push → GoReleaser (`.github/workflows/release.yml`) → GitHub
release + GHCR image. No manual steps between commit and tagged
artifact.

```sh
git tag -a v1.2.X -m "packyard v1.2.X"
git push origin v1.2.X
gh run watch    # ~2.5 min
```

**Tag convention:** Git tag is `vX.Y.Z`, GHCR image tag is `X.Y.Z`
(no `v` prefix). This is goreleaser's default `{{ .Version }}`
behaviour. `ghcr.io/schochastics/packyard:1.2.0` works;
`:v1.2.0` returns `manifest unknown`.

`:latest` is updated on every non-prerelease tag (`v1.3.0-rc.1` skips
it), and the release job only runs after `go test -race ./...` passes.

## Repo gotchas worth memorising

- **Home is `github.com/schochastics/packyard`** (`origin`). The repo
  briefly lived on gitea.cynkra.com (Sept 2026, module path
  `gitea.cynkra.com/david.schoch/packyard`); that was rolled back and the
  Gitea repo deleted.

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
- **`[skip ci]` in a commit message suppresses tag-triggered release
  runs too.** Learned on v1.1.0: the tag was created on a `[skip ci]`
  commit and Release never fired. Don't tag a `[skip ci]` commit.
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
- Fuzz targets for multipart publish live in
  `internal/api/publish_fuzz_test.go`; `.github/workflows/fuzz.yml` runs them on
  PRs touching `publish*.go`, `internal/store`, `internal/cas` or
  `internal/rpkg`, and weekly.
- `internal/metrics` has 0% coverage by design — it's definitions
  plus registrations, exercised transitively by `metrics_*_test.go`
  in `internal/api`.
- CRAN-protocol compliance is HTTP/byte-level in
  [cran_protocol_test.go](internal/api/cran_protocol_test.go);
  real R clients run in `make e2e` ([tests/e2e/](tests/e2e/)), which
  `.github/workflows/cran-e2e.yml` runs on path-filtered PRs, weekly,
  and manually before a release.

## Memory vs this file

- `CLAUDE.md` (this file) — **repo-anchored** facts. Survives `rm -rf`
  of the machine; versioned with the code.
- `~/.claude/projects/-Users-david-projects-pakman/memory/` —
  **session-scoped** memory (user profile, preferences, project state
  between sessions, feedback). Machine-local; not committed.

Put stable repo facts here. Put ephemeral working state and anything
about the user in memory.

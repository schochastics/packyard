# e2e test fixtures

Shared assets for end-to-end workflows under `.github/workflows/`.

## `testpkg/`

A minimal R package (no compiled code, no dependencies) used by
`cran-e2e.yml` as the payload for the `publish → install.packages()`
round trip. Edit cautiously — breakage here breaks the CI job.

Run locally:

```sh
R CMD build tests/e2e/testpkg
# produces testpkg_0.1.0.tar.gz in the current dir
```

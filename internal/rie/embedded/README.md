# internal/rie/embedded

This directory holds nothing but this README in the repository. During the
release workflow (`.github/workflows/release.yml`'s `binaries` job), CI
places a per-target `aws-lambda-rie` binary here — built from the pinned
upstream tag (`rie.Version` in `internal/rie/rie.go`) for the `GOOS`/`GOARCH`
about to be compiled — immediately before building `lambdary` with
`-tags embedrie`. `internal/rie/embedded.go`'s `//go:embed
embedded/aws-lambda-rie` directive then bakes that binary into the release
build, so `rie.Resolve` can extract it straight from the cache step instead
of shelling out to `git`/`go` on the end user's machine.

`aws-lambda-rie` (the binary itself) is gitignored — CI rebuilds it fresh for
every target and removes it between targets so a stale binary for the wrong
`GOOS`/`GOARCH` can never leak into the next one. A plain `go build` or
`go install` (no `embedrie` tag) never looks at this directory at all; see
`internal/rie/embedded_stub.go`.

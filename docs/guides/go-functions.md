---
description: Run Go Lambda functions without building a bootstrap by hand.
---

# Go Functions

A Go function needs no hand-built `bootstrap`. Lambdary builds it with `go build -tags lambda.norpc` every time the function starts, so hot reload rebuilds it too, and Go's build cache keeps an unchanged rebuild quick.

You need `go` on your `PATH`. The [Docker image](docker-compose.md) carries one.

## One function per directory

A function directory with a `go.mod` and a `provided.*` runtime is built this way. Since a `go.mod` on its own already implies `provided.al2023`, this is enough:

```
my-functions/
  checksum/
    go.mod
    main.go
```

A function with a `Dockerfile` still builds from its `Dockerfile`.

## Many functions in one module

For one module holding many functions, list them in `lambdary.yml`:

```yaml
functions:                # replaces the directory scan when set
  ping:
    go: ./cmd/ping        # a Go main package, relative to root
  notify:
    go: ./cmd/notify
    timeout: 30           # any .lambda.yml key, inline
    url:
      path: /hooks/notify
  legacy:
    path: ./legacy-node   # an ordinary function directory
build:
  go:
    tags: [netgo]         # added after lambda.norpc
    ldflags: "-X main.version=dev"
```

Each entry sets exactly one of `go` or `path`, and the key is the function's name. Inline settings win over a `.lambda.yml` in that directory, which wins over `defaults:`. A `go:` entry runs on `provided.al2023` unless the entry or its `.lambda.yml` says otherwise.

See [Project Configuration](../configuration/project-config.md#functions) for every rule.

## How builds work

- The build runs `go build` with `CGO_ENABLED=0` from the module that holds the package.
- The tags are `lambda.norpc`, then `build.go.tags`. `build.go.ldflags` is passed as `-ldflags`.
- The container backend builds for Linux on the function's `architectures` (`x86_64` by default). The process backend builds for your host.
- Binaries go to `.lambdary/build/`, never into your source tree.
- A failed build fails the function's start, with the tail of the compiler output.

## Reloads

Editing a function's own package reloads that function. Editing shared code elsewhere in the module, or `go.mod` / `go.sum`, reloads every function built from it.

## Published versions

[Publishing a version](versions-and-aliases.md) of a Go function freezes the `bootstrap` built for the backend that would run it at that moment. See [Limitations](../reference/limitations.md#go-functions) for what that means when you switch backends later.

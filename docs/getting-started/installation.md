---
description: Install Lambdary with Homebrew, Go, a prebuilt binary or the Docker image.
---

# Installation

## Requirements

- Linux or macOS, on `amd64` or `arm64`.
- Go 1.26 or higher, only when you install with `go install`.
- Optionally Docker, Podman, Finch or nerdctl, for the [container backend](how-it-works.md#two-execution-backends).

## Installing Lambdary

{% tabs %}
{% tab title="Homebrew" %}
Homebrew works on macOS and Linux:

```sh
brew install open-southeners/tap/lambdary
```
{% endtab %}

{% tab title="Go" %}
```sh
go install github.com/open-southeners/lambdary/cmd/lambdary@latest
```
{% endtab %}

{% tab title="Prebuilt binary" %}
Prebuilt binaries for Linux and macOS, `amd64` and `arm64`, are attached to each [GitHub release](https://github.com/open-southeners/lambdary/releases). Download the archive for your platform, extract it, and put `lambdary` somewhere on your `PATH`.
{% endtab %}

{% tab title="Docker" %}
The `ghcr.io/open-southeners/lambdary` image runs `lambdary dev` as a Docker Compose service:

```yaml
services:
  lambdary:
    image: ghcr.io/open-southeners/lambdary:latest
```

See [Docker Compose](../guides/docker-compose.md) for the full setup.
{% endtab %}
{% endtabs %}

Check the install:

```sh
lambdary --version
```

## The process backend's emulator

Both backends run your code through AWS's [Runtime Interface Emulator](https://github.com/aws/aws-lambda-runtime-interface-emulator). The container backend uses the copy inside AWS's base images. The process backend runs it as a host binary.

Release binaries, including the ones Homebrew installs, carry that emulator built in and write it to `~/.lambdary/bin/` on first use. A `go install` build doesn't, so the first process-backend start clones AWS's pinned emulator source and builds it, which needs `git` and `go` once. The result is cached in the same folder.

{% hint style="info" %}
`LAMBDARY_RIE_PATH` points Lambdary at an emulator binary you already have, and `LAMBDARY_HOME` moves `~/.lambdary`. See [Environment Variables](../configuration/environment-variables.md).
{% endhint %}

## Next steps

Head to the [Quick Start](quick-start.md) to scaffold and run your first function.

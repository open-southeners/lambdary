# Lambdary

[![Lambdary](https://opensoutheners.com/gh_card/lambdary?stats=version,platforms,license,ci&platforms=Linux,macOS&extra%5Bbrand%3Ago%5D=Go%20*1.26%2B*&extra%5Bbrand%3Adocker%5D=Docker%20*optional*)](https://docs.opensoutheners.com/lambdary/)

Local development server for AWS Lambda functions. Point it at a folder of
function directories and get one local HTTP server that runs each function on
the same runtimes AWS uses, through AWS's own Runtime Interface Emulator: in
Docker when you have it, on your host runtimes when you don't.

```
$ lambdary dev
lambdary dev server listening on http://127.0.0.1:8000
backend: container (docker)

  api     /api     (nodejs22.x)
  worker  /worker  (python3.13)
```

## Install

Homebrew (macOS and Linux):

```sh
brew install open-southeners/tap/lambdary
```

Go:

```sh
go install github.com/open-southeners/lambdary/cmd/lambdary@latest
```

Prebuilt binaries for Linux and macOS are attached to each
[GitHub release](https://github.com/open-southeners/lambdary/releases), and
the `ghcr.io/open-southeners/lambdary` image runs it in Docker Compose.

## Quick start

```sh
mkdir my-functions && cd my-functions
lambdary init api        # Node.js scaffold; also --runtime python3.13 or provided.al2023
lambdary dev
curl http://127.0.0.1:8000/api
```

Edit the handler and save: `lambdary dev` reloads it. The AWS CLI and SDKs
work against the same server:

```sh
aws lambda invoke --endpoint-url http://127.0.0.1:8000 --function-name api out.json
```

## Documentation

Configuration, versions and aliases, queues and schedules, metrics, Docker
Compose and everything else is in the
[official documentation](https://docs.opensoutheners.com/lambdary/).

`CHANGELOG.md` tracks user-facing changes, and `CONTRIBUTING.md` covers
development and releases.

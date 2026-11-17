---
description: A local development server for AWS Lambda functions that runs each function on the same runtimes AWS uses.
---

# Introduction

**Lambdary** is a local development server for AWS Lambda functions. Point it at a folder of function directories and get one local HTTP server that runs each function on the same runtimes AWS uses, through AWS's own [Runtime Interface Emulator](https://github.com/aws/aws-lambda-runtime-interface-emulator): in Docker when you have it, on your host runtimes when you don't.

```
$ lambdary dev
lambdary dev server listening on http://127.0.0.1:8000
backend: container (docker)

  api     /api     (nodejs22.x)
  worker  /worker  (python3.13)
```

Lambdary does not reimplement Lambda. It drives your code through AWS's own emulator and speaks AWS's own APIs, so the AWS CLI and every AWS SDK work against it unchanged.

## Features

- **Zero-config discovery** -- every function directory under your project root is found from its `.lambda.yml` or project file (`package.json`, `go.mod`, `requirements.txt`, ...).
- **Function URL routes** -- `http://localhost:8000/<function>/...` arrives in your handler as a real Lambda Function URL / API Gateway v2 event.
- **AWS-compatible invoke API** -- `aws lambda invoke --endpoint-url http://127.0.0.1:8000` works as is.
- **Two execution backends** -- AWS's official base images in Docker, Podman, Finch or nerdctl, or your host runtimes when no container runtime is around.
- **Hot reload** -- save a file and only that function restarts; config edits reconfigure the server live.
- **Go builds** -- Go functions are built for you on every start, no hand-built `bootstrap`.
- **Layers** -- layer version ARNs and local directories merged into `/opt`, as on AWS.
- **Versions and aliases** -- `publish-version`, `create-alias` and weighted canaries, locally.
- **SQS queues and schedules** -- a local SQS API, event source mappings with `ReportBatchItemFailures` and dead-letter queues, and EventBridge `rate()`, `cron()` and `at()` schedules.
- **Sample events** -- `lambdary invoke worker --event sqs` builds an SQS, S3, schedule or API Gateway event for you.
- **Logs, metrics and health** -- `lambdary logs`, a metrics summary, CloudWatch `GetMetricData`, and a health endpoint for Compose.
- **Docker Compose image** -- `ghcr.io/open-southeners/lambdary` runs the dev server next to the rest of your stack.

## Requirements

- Linux or macOS.
- For the container backend: Docker, Podman, Finch or nerdctl.
- For the process backend: the runtimes your functions use (`node`, `python3`, `ruby`, ...), plus `git` and `go` once, to build AWS's emulator when the binary does not already carry it.
- For Go functions: `go` on your `PATH`.

## Quick start

```sh
brew install open-southeners/tap/lambdary

mkdir my-functions && cd my-functions
lambdary init api
lambdary dev
curl http://127.0.0.1:8000/api
```

Then head to the [Installation](getting-started/installation.md) guide for every way to install Lambdary, or to the [Quick Start](getting-started/quick-start.md) for a short tour.

## Further reading

- [`DESIGN.md`](https://github.com/open-southeners/lambdary/blob/main/DESIGN.md) documents the architecture.
- [`CHANGELOG.md`](https://github.com/open-southeners/lambdary/blob/main/CHANGELOG.md) tracks user-facing changes.

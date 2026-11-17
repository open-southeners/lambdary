# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Go builds**: `provided.*` Go functions are built on every start and hot reload, no hand-built `bootstrap`.
- **Explicit function list**: `functions:` in `lambdary.yml` names each function by `go:` package or `path:`.
- `build.go.tags` and `build.go.ldflags` in `lambdary.yml` customise Go builds.
- Versions and aliases: `aws lambda publish-version`, `create-alias`, `update-alias` and `invoke --qualifier` work locally, including weighted canaries.
- Official Docker image, `ghcr.io/open-southeners/lambdary`, for running `lambdary dev` in Docker Compose.
- `LAMBDARY_NETWORK` puts function containers on a Docker network, and `LAMBDARY_HOST_ROOT` sets the host path of `--root`.
- `lambdary dev --host` sets the address the server listens on.
- **SQS queues**: `lambdary dev` serves SQS's API, so producers use the AWS SDK or CLI against it, and `events: [{sqs: …}]` delivers batches to a function with `ReportBatchItemFailures`, visibility-timeout retries and a dead-letter queue.
- **Schedules**: `events: [{schedule: "rate(1 minute)"}]`, in AWS's `rate()`, `cron()` and `at()` syntax, sends EventBridge's Scheduled Event while the server runs; `--no-schedules` turns them off.
- `queues:` in `lambdary.yml` declares queues that only producers use.
- **Metrics**: `GET /_lambdary/metrics?window=1h` summarises invocations, errors, throttles and durations per function.
- CloudWatch `GetMetricData` for `AWS/Lambda` works against the dev server, so an AWS SDK reads local metrics with `AWS_ENDPOINT_URL_CLOUDWATCH`.
- `GET /_lambdary/health` for Compose healthchecks.
- `lambdary logs [function] [-f] [--tail N]` prints a running dev server's function output, from `GET /_lambdary/logs`.
- **Event fixtures**: `lambdary invoke <fn> --event sqs|s3|schedule|apigw` builds a sample event, and `--set key=value` overrides its fields.
- Asynchronous invokes: `X-Amz-Invocation-Type: Event` answers `202` and retries a failing function twice, and `DryRun` answers `204`.

### Changed

- `Invoke` responses now carry `X-Amz-Executed-Version`, and `X-Amz-Function-Error` when the handler fails.
- Invoking an unknown function now reports `ResourceNotFoundException` to AWS SDKs.
- Schedules invoke asynchronously, so a failed scheduled invocation is retried.
- Function containers are no longer started with `--rm`; `Stop` removes them.

### Fixed

- The container backend pulls and runs images for the function's `architectures` (`--platform linux/<arch>`), and re-pulls a cached image of another platform.
- A container that exits at start now reports its exit code and last log lines, and the invoke API answers `502` `ServiceException`.

## [1.1.0] - 2026-09-06

### Added

- Lambda layer support.
- Correct local responses for streaming functions.

## [1.0.1] - 2026-08-18

### Changed

- Built release binaries with Go 1.26.6.

### Fixed

- Bodyless HTTP responses no longer fail.

## [1.0.0] - 2026-08-05

### Added

- First release

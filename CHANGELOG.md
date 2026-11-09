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

### Changed

- `Invoke` responses now carry `X-Amz-Executed-Version`, and `X-Amz-Function-Error` when the handler fails.
- Invoking an unknown function now reports `ResourceNotFoundException` to AWS SDKs.

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

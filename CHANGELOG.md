# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [1.2.0] - 2026-11-17

### Added

- Go functions build automatically on start and on every hot reload.
- `functions:` in `lambdary.yml` lists functions by Go package or path.
- `build.go.tags` and `build.go.ldflags` customise Go builds.
- Function versions and aliases, including weighted canaries.
- SQS queues and `sqs` event sources, with retries and dead-letter queues.
- Schedules in AWS's `rate()`, `cron()` and `at()` syntax.
- Asynchronous invokes with AWS-style retries, and `DryRun`.
- Invocation metrics, also readable through CloudWatch `GetMetricData`.
- `lambdary logs` to follow a running dev server's function output.
- `lambdary invoke --event sqs|s3|schedule|apigw` builds sample events.
- `get-function`, `get-function-configuration` and `list-functions`.
- `GET /_lambdary/health` for Docker Compose healthchecks.
- Official Docker image, `ghcr.io/open-southeners/lambdary`.
- `lambdary dev --host` sets the address the server listens on.
- `LAMBDARY_ROOT`, `LAMBDARY_NETWORK` and `LAMBDARY_HOST_ROOT` settings.
- MIT license.

### Changed

- Functions without `timeout` get 900 s, AWS's maximum, instead of 30 s.
- `timeout: 0` means no limit locally, and values outside 1-900 s warn.
- Invokes return `X-Amz-Executed-Version` and `X-Amz-Function-Error`, as on AWS.
- Unknown functions report `ResourceNotFoundException` to AWS SDKs.
- Unsupported AWS API calls answer `UnsupportedOperation` instead of a 404.

### Fixed

- Function images run on the platform set by `architectures`.
- A function that fails to start reports its exit code and last log lines.
- An `Invoke` with no payload reaches the handler as `{}`, as on AWS.
- Functions without `memory` report 128 MB, AWS's default.

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

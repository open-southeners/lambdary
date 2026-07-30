# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Function discovery**: every direct subdirectory of the project root that
  contains a `.lambda.yml` or a recognizable project file (`package.json`,
  `composer.json`, `pyproject.toml`/`requirements.txt`, `go.mod`, `Gemfile`,
  `*.csproj`, `Dockerfile`) is picked up as a Lambda function, with its
  runtime inferred from that marker; a `.lambda.yml` can always override the
  inferred runtime.
- **`.lambda.yml` manifest**: per-function configuration that mirrors AWS
  Lambda's own vocabulary (`runtime`, `handler`, `timeout`, `memory`,
  `architectures`, `environment`, `url`) plus a local-only `local` section
  (`backend`, `image`, `command`, `env_file`) for how the function runs on
  your machine. A project-wide `lambdary.yml` can set `port`, the functions
  `root`, and `defaults` inherited by every function manifest. Unknown keys
  in either file are rejected so typos surface immediately instead of being
  silently ignored.
- **`lambdary list` command**: prints every discovered function with its
  runtime, backend, and local route. Misconfigurations — route collisions,
  duplicate function names, unresolvable runtimes, invalid manifests — are
  reported as warnings alongside the table rather than aborting the listing,
  so a problem in one function never hides the rest.
- macOS groundwork: confirmed AWS's own Lambda Runtime Interface Emulator
  (pinned upstream tag `v1.35`) builds and runs natively on Apple Silicon, so
  local (non-container) function execution will run on AWS's real emulator
  instead of a reimplementation.

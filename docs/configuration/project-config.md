---
description: Every key of the project-wide lambdary.yml, with defaults.
---

# Project Configuration

An optional `lambdary.yml` directly under `--root` (the current directory by default) configures the whole project:

```yaml
port: 8000
root: .                   # where the function directories live
defaults:                 # inherited by functions that don't set them
  timeout: 15
  environment:
    STAGE: local
```

Unknown keys are rejected, so typos surface immediately. Unlike a function's `.lambda.yml`, an invalid `lambdary.yml` is an error: the command stops and names the problem.

## Keys

| Key | Default | Description |
|---|---|---|
| `port` | `8000` | Port of the dev server. `lambdary dev --port`, `invoke --port` and `logs --port` override it. Must be between 1 and 65535. |
| `root` | `.` | Where the function directories live, relative to `lambdary.yml`. |
| `defaults` | none | Settings inherited by functions that don't set them. |
| `functions` | none | An explicit function list that replaces the directory scan. |
| `build.go.tags` | none | Extra Go build tags, added after `lambda.norpc`. |
| `build.go.ldflags` | none | Passed to `go build -ldflags` as is. |
| `queues` | none | Local SQS queues to create at startup. |
| `aws.environment` | `false` | Give every function the AWS variables it needs to call Lambdary. See [`aws`](#aws). |
| `aws.region` | `us-east-1` | Region reported to functions. Lowercase letters, digits and hyphens, such as `eu-south-2`. |

## `defaults`

`defaults:` takes the same keys as a `.lambda.yml`, except `name` and `events`: `runtime`, `handler`, `timeout`, `memory`, `architectures`, `layers`, `environment`, `url` and `local`. It is validated with the same rules.

A function inherits a default only for a key it doesn't set itself. Each key is taken whole: a function with its own `environment` map does not merge in `defaults.environment`. See [precedence](function-config.md#precedence).

## `functions`

By default Lambdary scans `root` for function directories. For one module holding many functions, such as a Go module with `cmd/<name>` packages, list them instead:

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

- The map key is the function's name. It must not be empty or contain `/` or whitespace, and an entry must not set `name`.
- Each entry sets exactly one of `go` or `path`, relative to `root`.
- `go:` is a Go main package. Lambdary looks for the `go.mod` at or above it and builds the function's `bootstrap` from that module. A `go:` entry runs on `provided.al2023` with handler `bootstrap`, unless the entry or its `.lambda.yml` says otherwise. Project `defaults:` don't change a `go:` entry's runtime or handler.
- `path:` is an ordinary function directory, handled like a discovered one. It's a function even without a `.lambda.yml` or project file.
- Every other key is a `.lambda.yml` key, written inline. Inline settings win over a `.lambda.yml` in that directory, which wins over `defaults:`.

A listed directory that doesn't exist, or a `go:` package with no `go.mod` above it, is an error.

See [Go Functions](../guides/go-functions.md) for how builds and reloads work.

## `build.go`

| Key | Default | Description |
|---|---|---|
| `build.go.tags` | none | Build tags added after the `lambda.norpc` tag Lambdary always sets. |
| `build.go.ldflags` | none | Passed to `go build -ldflags` as is. |

These apply to every Go function: `go:` entries and scanned `provided.*` directories with a `go.mod`.

## `aws`

```yaml
aws:
  environment: true       # default false: functions get no extra variables
  region: eu-south-2      # default us-east-1
```

With `aws.environment: true`, every function gets the region, placeholder credentials and an endpoint pointing at the dev server, so an AWS SDK or the AWS CLI inside it calls Lambdary with no setup. A variable the function's own `environment` or `local.env_file` sets is never replaced. Off by default, functions get exactly what they did before.

The variables are listed in [Environment Variables](environment-variables.md#aws-variables-injected-with-awsenvironment). Changes to `aws` apply on the next start of `lambdary dev`, not on hot reload. See [Using AWS SDKs and the CLI](../guides/using-aws-sdks.md#from-inside-a-function).

## `queues`

Queues that only producers use, or whose attributes you want in one place, can be declared here:

```yaml
queues:
  probe-results: {visibility_timeout: 60}
  deploy-requests: {dlq: deploy-requests-dlq, max_receive_count: 5}
```

| Key | Default | Description |
|---|---|---|
| `visibility_timeout` | `30` | Seconds a received message stays hidden, 0 to 43200. |
| `delay_seconds` | `0` | Seconds every new message is delayed, 0 to 900. |
| `dlq` | none | Dead-letter queue name. Needs `max_receive_count`. |
| `max_receive_count` | none | Receives after which a message moves to `dlq`, 1 to 1000. Needs `dlq`. |

Queue names are 1 to 80 letters, digits, `-` or `_`. A queue can't be its own `dlq`. See [Queues and Schedules](../guides/queues-and-schedules.md).

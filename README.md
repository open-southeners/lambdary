# Lambdary

Local development server for AWS Lambda functions. Point it at a folder of
function directories and get one local HTTP server that runs each function on
the same runtimes AWS uses — via Docker when you have it, directly on your
host runtimes when you don't.

```
$ lambdary dev
lambdary dev server listening on http://127.0.0.1:8000
backend: container (docker)

  api     /api     (nodejs22.x)
  worker  /worker  (python3.13)
```

## How it works

Every direct subdirectory of your project root that contains a `.lambda.yml`
or a recognizable project file (`package.json`, `composer.json`,
`pyproject.toml`/`requirements.txt`, `go.mod`, `Gemfile`, `*.csproj`,
`Dockerfile`) is a function. Its directory name becomes its name and its
local route. Layouts a directory scan can't express, such as one Go module
with many `cmd/<name>` functions, can list their functions explicitly instead
(see [Go functions](#go-functions)).

Requests to `http://localhost:8000/<function>/...` arrive in your handler as
real **AWS Lambda Function URL / API Gateway v2 events** — headers, cookies,
query strings, and binary bodies mapped both directions — and shaped
responses (`statusCode`, `headers`, `cookies`, `isBase64Encoded`) are honored
on the way out, exactly like Lambda Function URLs.

The server also exposes the real AWS invoke endpoint, so the AWS CLI and any
AWS SDK work against it unchanged:

```sh
curl -X POST http://127.0.0.1:8000/2015-03-31/functions/api/invocations -d '{"any":"event"}'
aws lambda invoke --endpoint-url http://127.0.0.1:8000 --function-name api out.json
```

### Two execution backends

| | Container (default when available) | Process (fallback) |
|---|---|---|
| Runs on | AWS's official `public.ecr.aws/lambda/*` base images | Your host-installed runtimes (`node`, `python3`, `ruby`, …) |
| Requires | Docker, Podman, Finch, or nerdctl | `git` + `go` once, to build AWS's emulator from pinned source |
| Fidelity | Amazon Linux, the real deal | Real Lambda Runtime API semantics, host OS and runtime versions |

Both backends drive your code through AWS's own
[Lambda Runtime Interface Emulator](https://github.com/aws/aws-lambda-runtime-interface-emulator) —
Lambdary does not reimplement Lambda. With `--backend auto` (the default),
Lambdary uses containers when a runtime is up and falls back to the process
backend with a notice when it isn't. The process backend supports Node,
Python, Ruby, and custom runtimes (any `provided.*` function with a
`bootstrap` executable — Bref-style PHP works out of the box). A function
whose runtime has no process-backend shim (Java, .NET, …) still runs: it
falls back to the container backend automatically, with a notice, so
`--backend process`/`auto` never fail a function outright just because
that language isn't shimmed yet.

## Getting started

```sh
go install github.com/open-southeners/lambdary/cmd/lambdary@latest

mkdir my-functions && cd my-functions
lambdary init api                      # nodejs22.x scaffold (also: --runtime python3.13 | provided.al2023)
lambdary dev
curl http://127.0.0.1:8000/api
```

Edit your handler and just save — `dev` hot-reloads: code edits restart only
that function, adding/removing function directories or editing config
reconfigures the server live, and a broken edit keeps the previous
configuration running instead of taking the server down. Function output
(including Lambda-style `REPORT` lines per invocation) streams to your
terminal with a color-coded `[name]` prefix.

## Commands

```
lambdary dev      [--port N] [--backend auto|container|process] [--eager] [--no-reload] [--no-schedules]
lambdary list                                # discovered functions, runtime, route
lambdary invoke <fn> [-e event.json|-]       # one-shot invoke (uses the dev server if running)
lambdary init <name> [--runtime <id>]        # scaffold a function
```

All commands accept `--root <dir>` (default `.`).

## Configuration

Per-function `.lambda.yml` — every key optional, names mirror AWS Lambda's
own configuration:

```yaml
name: api                 # default: directory name
runtime: nodejs22.x       # any AWS runtime identifier
handler: handler.handler
timeout: 30               # seconds
memory: 512               # MB
architectures: [arm64]
environment:
  TABLE_NAME: local-table
url:
  path: /api              # local route, default /<name>
  payload: "2.0"          # event format (Function URL / API Gateway v2)
  invoke_mode: BUFFERED   # or RESPONSE_STREAM; parses a streamifyResponse frame into real status/headers/cookies
layers:                   # up to 5, merged into /opt in order (later entries win)
  - arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1  # a layer version ARN (e.g. Bref's PHP layer)
  - ../shared-layer        # a local directory or .zip, relative to the function dir
local:                    # local-development-only section
  backend: auto           # auto | container | process
  image: ""               # container image override
  command: ""             # process-backend command override
  env_file: ""            # extra environment from a dotenv-style file (explicit `environment` keys win)
```

`layers:` mirrors AWS Lambda's own Layers feature, not a local-only hint, so
it lives at the top level like `runtime` or `environment`. Up to 5 entries,
each a layer version ARN or a local path, merged in declared order into
`/opt` — later entries win on file conflicts, exactly like AWS. ARN entries
are fetched using your own AWS credentials (the standard SDK credential
chain), which is enough to pull public third-party layers too, such as
Bref's PHP layers; fetched content is cached under `.lambdary/` and
digest-pinned in `.lambdary/lock` so a function keeps starting from the
same layer version across machines. Local path entries (a directory or a
`.zip`, resolved relative to the function directory) need no AWS
credentials at all. On the container backend, a Bref-style `provided.*`
function can now get its `bootstrap` straight from a layer — no local
`bootstrap` file required, matching AWS's own bootstrap search order.

Project-wide `lambdary.yml` next to your functions:

```yaml
port: 8000
root: .                   # where the function directories live
defaults:                 # inherited by functions that don't set them
  timeout: 15
  environment:
    STAGE: local
```

Unknown keys in either file are rejected, so typos surface immediately.

### Go functions

A Go function needs no hand-built `bootstrap`. Lambdary builds it with
`go build -tags lambda.norpc` every time the function starts, so hot reload
rebuilds it too, and Go's build cache keeps an unchanged rebuild quick. The
container backend builds for Linux on the function's `architectures`, and the
process backend builds for your host. Binaries go to `.lambdary/build/`, never
into your source tree. You need `go` on your `PATH`.

A function directory with a `go.mod` and a `provided.*` runtime is built this
way (a `Dockerfile` function still builds from its `Dockerfile`). For one
module holding many functions, list them in `lambdary.yml`:

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

Each entry sets exactly one of `go` or `path`, and the key is the function's
name. Inline settings win over a `.lambda.yml` in that directory, which wins
over `defaults:`. A `go:` entry runs on `provided.al2023` unless the entry or
its `.lambda.yml` says otherwise. Editing a function's own package reloads
that function; editing shared code elsewhere in the module (or `go.mod` /
`go.sum`) reloads every function built from it.

### Versions and aliases

`lambdary dev` serves Lambda's version and alias API, so the AWS CLI and SDKs
work unchanged with an endpoint override:

```sh
export AWS_ENDPOINT_URL=http://127.0.0.1:8000

aws lambda publish-version --function-name notify --description "sha=abc123"
aws lambda create-alias --function-name notify --name live --function-version 1

# Send 10% of live's traffic to version 2
aws lambda update-alias --function-name notify --name live \
  --routing-config '{"AdditionalVersionWeights":{"2":0.1}}'

aws lambda invoke --function-name notify --qualifier live out.json
```

Supported calls are `PublishVersion`, `ListVersionsByFunction`, `CreateAlias`,
`GetAlias`, `UpdateAlias`, and `Invoke` with a version number or alias as its
qualifier. Each invoke response names the version that ran in
`X-Amz-Executed-Version`.

A version is an immutable copy of the function under `.lambdary/versions/`, so
it survives restarts and keeps running the same code however you edit the
source. Only `$LATEST`, the unqualified function, hot reloads. As on AWS,
publishing with nothing changed returns the latest version instead of a new one.

A snapshot includes everything `.lambda.yml` resolved, including values from
`local.env_file` in plain text, so keep `.lambdary/versions/` out of version
control. Function routes (`url.path`) always run `$LATEST`.

### Queues and schedules

Functions can declare the event sources that invoke them on AWS, and
`lambdary dev` runs them locally:

```yaml
# .lambda.yml, or inline in a lambdary.yml functions entry
events:
  - sqs:
      queue: notifications          # created when the server starts
      batch_size: 10                # 1-10, or up to 10000 with batch_window
      batch_window: 0               # seconds to gather a fuller batch
      function_response_types: [ReportBatchItemFailures]
      visibility_timeout: 30        # queue settings, applied to the queue
      dlq: notifications-dlq
      max_receive_count: 5
  - schedule: rate(1 minute)        # or cron(0 9 ? * MON-FRI *), at(2026-12-01T09:00:00)
  - schedule:
      expression: cron(0 9 * * ? *)
      timezone: Europe/Madrid       # default UTC
      input: '{"kind":"digest"}'    # sent instead of the Scheduled Event
      enabled: false
```

The dev server answers SQS's API on its own port, so producers use the real
AWS SDK or CLI with `AWS_ENDPOINT_URL_SQS=http://127.0.0.1:8000` (or
`http://lambdary:8000` from Compose). It supports `CreateQueue`, `GetQueueUrl`,
`ListQueues`, `DeleteQueue`, `PurgeQueue`, `Get/SetQueueAttributes`,
`SendMessage[Batch]`, `ReceiveMessage` with long polling, `DeleteMessage[Batch]`
and `ChangeMessageVisibility[Batch]`. Queues only producers use can be declared
in `lambdary.yml`:

```yaml
queues:
  probe-results: {visibility_timeout: 60}
  deploy-requests: {dlq: deploy-requests-dlq, max_receive_count: 5}
```

An `sqs` event polls its queue and invokes the function with real SQS event
JSON, as an event source mapping does. Messages the function handled are
deleted. After a function error, the whole batch comes back after the
visibility timeout. With `ReportBatchItemFailures`, only the IDs in
`batchItemFailures` come back. A message received more than
`max_receive_count` times moves to the `dlq`.

A schedule sends EventBridge's `Scheduled Event` each time its expression
fires, in AWS's `rate()`, six-field `cron()` and `at()` syntax. `--no-schedules`
turns them off. Both kinds accept `qualifier:` to invoke a version or alias
instead of `$LATEST`.

### Running in Docker Compose

The `ghcr.io/open-southeners/lambdary` image runs `lambdary dev` as a Compose
service, starting function containers next to it through the Docker socket:

```yaml
services:
  lambdary:
    image: ghcr.io/open-southeners/lambdary:latest
    command: ["--root", "functions"]        # added to `lambdary dev`
    user: "1000:1000"                       # your host user, so files stay yours
    group_add: ["988"]                      # the docker socket's group
    stop_grace_period: 40s                  # time to stop function containers
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - .:/workspace
      - lambdary-cache:/cache               # Go's build and module caches
volumes:
  lambdary-cache:
```

Other services reach it by name, such as `http://lambdary:8000/my-function` or
an AWS SDK with `http://lambdary:8000` as its endpoint. Functions join the same
network, so they reach those services by name too, and call each other through
that same endpoint. `stat -c %g /var/run/docker.sock` prints the group to use.

It works out the rest from its own container: which host paths its mounts come
from, and which network it is on. Two settings override that, and you need them
if it can't inspect itself, such as when you set `hostname:` on the service:

| Setting | Meaning |
|---|---|
| `LAMBDARY_NETWORK` | Docker network for function containers. Required when the service is on several networks. Also works outside a container. |
| `LAMBDARY_HOST_ROOT` | Host path of the `--root` directory. |

The image includes Go and downloads the Go version your project asks for. Only
the container backend runs from it, because it has no Node, Python or Ruby. The
Docker socket gives this service full control of the host's Docker, so use it
for local development only. Outside a container, `lambdary dev --host 0.0.0.0`
listens beyond loopback, with no authentication.

## Notes and limitations

- The process backend runs your code on your host's runtime versions —
  faithful to the Lambda Runtime API, not to Amazon Linux. Use the container
  backend when fidelity matters.
- `layers:` on the process backend works for pure-code layers, resolved
  through the runtimes' own standard search paths (`NODE_PATH`, `PYTHONPATH`,
  `RUBYLIB`/`GEM_PATH`, `PATH`) instead of a real `/opt`. Layer content
  compiled for Amazon Linux (native binaries, Bref's `php`) needs the
  container backend, same fidelity trade-off as the bullet above; a
  `provided.*` function whose `bootstrap` comes from a layer falls back to
  the container backend automatically, with a notice, since it can't run as
  a host process at all.
- Requests to one function are serialized (the emulator processes one
  invocation at a time), matching single-instance Lambda semantics.
- Route or name collisions never prevent startup: healthy functions keep
  serving and the colliding route answers `409` naming the competitors.
- `url.payload: "1.0"` selects API Gateway REST API events per function
  instead of the `"2.0"` default; a function with a `Dockerfile` and no
  `runtime`/`local.image` is built automatically (via `docker build`) on
  every start, including hot reload.
- `url.invoke_mode: RESPONSE_STREAM` parses a `streamifyResponse` handler's
  `http-integration-response` frame into its real status code, headers, and
  cookies instead of leaking the frame into the body — but the response is
  still buffered by the emulator end to end, so there's no
  time-to-first-byte benefit locally, only a correct one-shot response.
- Local queues are standard queues held in memory: they survive a hot
  reload but not a restart. FIFO queues and SQS's older query protocol
  (`Action=SendMessage` form posts) are not supported. Schedules fire only
  while the server runs, and a failed scheduled invocation is not retried.
- `.lambdary/lock` pins the container backend's image digests on first
  start — commit it alongside your functions for reproducible starts across
  machines.

`DESIGN.md` documents the architecture; `plans/` holds the per-milestone
implementation plans; `CHANGELOG.md` tracks user-facing changes.

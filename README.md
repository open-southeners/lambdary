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
local route.

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
lambdary dev      [--port N] [--backend auto|container|process] [--eager] [--no-reload]
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
local:                    # local-development-only section
  backend: auto           # auto | container | process
  image: ""               # container image override
  command: ""             # process-backend command override
  env_file: ""            # extra environment from a dotenv-style file (explicit `environment` keys win)
```

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

## Notes and limitations

- The process backend runs your code on your host's runtime versions —
  faithful to the Lambda Runtime API, not to Amazon Linux. Use the container
  backend when fidelity matters.
- Requests to one function are serialized (the emulator processes one
  invocation at a time), matching single-instance Lambda semantics.
- Route or name collisions never prevent startup: healthy functions keep
  serving and the colliding route answers `409` naming the competitors.
- `url.payload: "1.0"` selects API Gateway REST API events per function
  instead of the `"2.0"` default; a function with a `Dockerfile` and no
  `runtime`/`local.image` is built automatically (via `docker build`) on
  every start, including hot reload.
- `.lambdary/lock` pins the container backend's image digests on first
  start — commit it alongside your functions for reproducible starts across
  machines.

`DESIGN.md` documents the architecture; `plans/` holds the per-milestone
implementation plans; `CHANGELOG.md` tracks user-facing changes.

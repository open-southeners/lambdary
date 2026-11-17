---
description: How Lambdary discovers functions, turns HTTP requests into Lambda events and runs your code.
---

# How It Works

## Function discovery

Every direct subdirectory of your project root that contains a `.lambda.yml` or a recognizable project file is a function. Its directory name becomes its name and its local route. Directories whose name starts with `.` are skipped.

When `.lambda.yml` sets no `runtime`, the first project file found picks one:

| Project file | Runtime |
|---|---|
| `Dockerfile` | none: built and run as a container image (see below) |
| `package.json` | `nodejs22.x` |
| `composer.json` | `provided.al2023` |
| `pyproject.toml` or `requirements.txt` | `python3.13` |
| `go.mod` | `provided.al2023` |
| `Gemfile` | `ruby3.3` |
| `*.csproj` | `dotnet8` |

A `Dockerfile` wins over every other file. A function with a `Dockerfile` and no `runtime` or `local.image` runs on the container backend and is built with `docker build` on every start, hot reload included.

A `provided.*` function with its own `go.mod` is a [Go function](../guides/go-functions.md): Lambdary builds its `bootstrap` for you.

Layouts a directory scan can't express, such as one Go module with many `cmd/<name>` functions, can list their functions explicitly in `lambdary.yml` instead. See [Project Configuration](../configuration/project-config.md#functions).

`lambdary list` shows what was discovered, with each function's runtime, backend, route and directory, plus any warnings:

```sh
lambdary list
```

## Function routes

Requests to `http://localhost:8000/<function>/...` arrive in your handler as real **AWS Lambda Function URL / API Gateway v2 events**. Headers, cookies, query strings and binary bodies are mapped in both directions. The function's route prefix is stripped from the event's `rawPath`, so `GET /api/users` reaches the `api` function with `rawPath` `/users`.

On the way out, shaped responses (`statusCode`, `headers`, `cookies`, `body`, `isBase64Encoded`) are honored exactly like Lambda Function URLs. A response without that shape is serialized as JSON with status `200`, as Function URLs do.

| Situation | HTTP status |
|---|---|
| The handler threw or returned an error | `502` with the error payload |
| The function timed out | `504` |
| The function failed to start, or returned a malformed response | `502` |
| Two functions share the route | `409`, naming the competitors |
| No function matches | `404` |

A function's route defaults to `/<name>`. `url.path` changes it, and `url.payload: "1.0"` switches that function to API Gateway REST API events. See [Function Configuration](../configuration/function-config.md#url).

`GET /` on the server answers a JSON list of every function with its name, route, runtime and backend.

## The invoke endpoint

The server also exposes the real AWS invoke endpoint, `POST /2015-03-31/functions/<name>/invocations`. It forwards the body as the raw event, so the AWS CLI and any AWS SDK work against it unchanged:

```sh
curl -X POST http://127.0.0.1:8000/2015-03-31/functions/api/invocations -d '{"any":"event"}'
aws lambda invoke --endpoint-url http://127.0.0.1:8000 --function-name api out.json
```

Use it for function-to-function calls and for non-HTTP event shapes such as SQS or S3. An invoke with no payload reaches the handler as `{}`, as on AWS. Beyond invoke, the server answers a set of Lambda, SQS and CloudWatch calls; see [AWS API](../reference/aws-api.md).

## Two execution backends

|  | Container (default when available) | Process (fallback) |
|---|---|---|
| Runs on | AWS's official `public.ecr.aws/lambda/*` base images | Your host-installed runtimes (`node`, `python3`, `ruby`, ...) |
| Requires | Docker, Podman, Finch, or nerdctl | `git` + `go` once, to build AWS's emulator from pinned source |
| Fidelity | Amazon Linux, the real deal | Real Lambda Runtime API semantics, host OS and runtime versions |

Both backends drive your code through AWS's own [Lambda Runtime Interface Emulator](https://github.com/aws/aws-lambda-runtime-interface-emulator). Lambdary does not reimplement Lambda.

The process backend's `git` + `go` requirement only applies to builds that don't already carry the emulator; release binaries do. See [Installation](installation.md#the-process-backends-emulator).

### Choosing a backend

With `--backend auto` (the default), Lambdary uses containers when a container runtime is up, probing `docker`, `podman`, `finch` and `nerdctl` in that order. When none is, it falls back to the process backend with a notice.

A function can pin its own backend with `local.backend: container` or `local.backend: process` in its `.lambda.yml`. `--backend` only sets the default for functions whose `local.backend` is `auto` or unset, so functions with different needs can share one dev server.

### Container backend

The container backend runs each function in AWS's base image for its runtime, such as `public.ecr.aws/lambda/nodejs:22` or `public.ecr.aws/lambda/provided:al2023`. The function directory is mounted read-only at `/var/task`, so a code change needs no image rebuild. `local.image` overrides the image.

Containers run for the function's `architectures`: `linux/amd64` for the default `x86_64`, `linux/arm64` for `arm64`. A cached image for another platform is pulled again. A container that exits at start fails with its exit code and last log lines.

`memory`, when set, caps the container's memory.

### Process backend

The process backend runs AWS's emulator as a host process and your code on your host's runtimes:

- Node, Python and Ruby functions run through small built-in shims that speak the Lambda Runtime API.
- Custom runtimes, any `provided.*` function with an executable `bootstrap`, run that `bootstrap`. Bref-style PHP works out of the box.
- `local.command` replaces the runtime command for a function. It runs with `sh -c` in the function directory.

A function whose runtime has no process-backend shim (Java, .NET, ...) still runs: it falls back to the container backend automatically, with a notice. So `--backend process` or `auto` never fails a function outright just because its language isn't shimmed yet.

The process backend is faithful to the Lambda Runtime API, not to Amazon Linux. Use the container backend when fidelity matters.

## Lifecycle

- **Lazy start.** A function's backend starts on its first invocation. `lambdary dev --eager` starts every function up front.
- **One invocation at a time.** Requests to one function are serialized, because the emulator processes one invocation at a time. This matches single-instance Lambda semantics.
- **Timeouts.** Each invocation is bounded by the function's `timeout`, 900 seconds unless you set one.
- **Hot reload.** Code edits restart only the affected function. Config edits and added or removed function directories reconfigure the server. A broken edit keeps the previous configuration running.
- **Fail-safe startup.** Route or name collisions never prevent startup: healthy functions keep serving and the colliding route answers `409`.
- **Shutdown.** `Ctrl-C` (SIGINT) or SIGTERM stops every running function before the server exits. Function containers carry a `lambdary=1` label, so `docker ps --filter label=lambdary=1` finds leftovers.

## The `.lambdary` folder

Lambdary keeps its project state in a `.lambdary/` folder next to your functions, under `--root`:

| Path | Contents | Commit it? |
|---|---|---|
| `.lambdary/lock` | Container image digests and layer digests, pinned on first start | Yes, for reproducible starts across machines |
| `.lambdary/build/` | Go `bootstrap` binaries | No |
| `.lambdary/layers/` | Fetched layer content | No |
| `.lambdary/versions/` | Published function versions, including resolved environment values | No |

Machine-wide state, such as the process backend's emulator binary, lives in `~/.lambdary/` (or `$LAMBDARY_HOME`).

{% hint style="warning" %}
`.lambdary/versions/` holds snapshots that include values from `local.env_file` in plain text. Keep it out of version control.
{% endhint %}

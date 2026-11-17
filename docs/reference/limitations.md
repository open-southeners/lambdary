---
description: Where Lambdary differs from AWS Lambda, and known gaps.
---

# Limitations

## Fidelity

- The process backend runs your code on your host's runtime versions: faithful to the Lambda Runtime API, not to Amazon Linux. Use the container backend when fidelity matters.
- `layers:` on the process backend works for pure-code layers, resolved through the runtimes' own standard search paths (`NODE_PATH`, `PYTHONPATH`, `RUBYLIB`/`GEM_PATH`, `PATH`) instead of a real `/opt`. Layer content compiled for Amazon Linux (native binaries, Bref's `php`) needs the container backend. A `provided.*` function whose `bootstrap` comes from a layer falls back to the container backend automatically, with a notice, since it can't run as a host process at all.
- Java and .NET have no process-backend shim. Those functions fall back to the container backend automatically, with a notice.
- The container backend runs each function's image for its `architectures` (`linux/amd64` for the default `x86_64`, `linux/arm64` for `arm64`), pulling it again when the cached image is for another platform. A container that exits at start fails with its exit code and last log lines, and the invoke API answers `502` `ServiceException`.

## Timeouts

A function's `timeout` defaults to 900 s, so a slow handler that would exceed a shorter limit you configure on AWS still passes locally unless you set that limit here too. `timeout: 0` (no limit) and values above 900 are outside AWS Lambda's 1-900 s range and warn at startup.

## Concurrency

Requests to one function are serialized (the emulator processes one invocation at a time), matching single-instance Lambda semantics. There is no scale-out to several instances.

## Routes and payloads

- Route or name collisions never prevent startup: healthy functions keep serving and the colliding route answers `409` naming the competitors.
- `url.payload: "1.0"` selects API Gateway REST API events per function instead of the `"2.0"` default.
- A `RESPONSE_STREAM` function that inherits `url.payload: "1.0"` from `lambdary.yml`'s `defaults:` is not rejected, and its responses are served as buffered payload 1.0 responses. Set both keys in the same file to get the validation error.

## Response streaming

`url.invoke_mode: RESPONSE_STREAM` parses a `streamifyResponse` handler's `http-integration-response` frame into its real status code, headers and cookies instead of leaking the frame into the body. But the response is still buffered by the emulator end to end, so there's no time-to-first-byte benefit locally, only a correct one-shot response.

## Builds

- A function with a `Dockerfile` and no `runtime`/`local.image` is built automatically (via `docker build`) on every start, including hot reload.
- Go functions are built with the host's `go` on both backends. With no `go` on `PATH`, every Go function fails to start, naming the failed `go build` command. The Docker image carries Go.
- .NET functions detected from a `*.csproj` are not built for you: the container runs whatever you last published by hand. Use a `Dockerfile` or publish the function yourself.

## Go functions

A published version freezes one `bootstrap`, built for the backend that would run the function at publish time. Where host and container differ (macOS, or a container architecture other than the host's), changing `--backend` or `local.backend` afterwards runs a binary built for the wrong platform, and the failure only appears at invoke time.

## Versions

- There is no way to delete a version. Snapshots are full copies of the function, including dependencies such as `node_modules`, and they accumulate under `.lambdary/versions/`.
- A version's container or process keeps running until the dev server stops or reconfigures, once it has been invoked.
- An `Event` invoke through a weighted alias picks its version once; its retries run that same version. AWS routes each retry again.
- After upgrading Lambdary, publishing an unchanged function once may create one extra version.

## Queues and schedules

- Local queues are standard queues held in memory: they survive a hot reload but not a restart, dead-letter queues included.
- FIFO queues and SQS's older query protocol (`Action=SendMessage` form posts) are not supported.
- Schedules fire only while the server runs. The `cron()` modifiers `L`, `W` and `#` are not supported.

## Async invokes

A failed asynchronous invoke is retried twice, after 1 s and 2 s. AWS waits about 1 and 2 minutes.

## Metrics

- Metrics are kept in memory and start over when the server restarts.
- The `Throttles` metric reads 0 in practice. An invocation only counts as throttled when its deadline has passed by the time it gets its turn, and since every invoke of a function has the same timeout, a waiting request almost always gets its turn in time.

## ARNs and region

ARNs in Lambda and SQS responses, errors, queue ARNs and the SQS, schedule and sample events always use region `us-east-1` and account `000000000000`, whatever region the client signed for. Code that parses or compares the region in an ARN sees `us-east-1`.

## Containers left behind

- Function containers are separate from Lambdary, so Docker doesn't stop them with it. A SIGKILL, including Compose's after a too-short `stop_grace_period`, leaves them running with no owner. `docker compose down` won't remove them, and they can keep the network in use, failing its removal.
- After a hard crash of `lambdary dev`, stopped function containers stay behind as exited containers.

Both carry the `lambdary=1` label, plus `lambdary.function=<name>` and `lambdary.dir=<function directory>`, so you can find and remove them:

```sh
docker ps -a --filter label=lambdary=1
docker rm -f $(docker ps -aq --filter label=lambdary=1)
```

{% hint style="warning" %}
The `lambdary=1` label is shared by every project on the same Docker daemon. Stop any other running `lambdary dev` first, or filter on `lambdary.dir` too.
{% endhint %}

## Security

Lambdary is for local development only. The server has no authentication: `lambdary dev --host 0.0.0.0` listens beyond loopback, and the Docker image's socket mount gives the service full control of the host's Docker.

## Reproducibility

`.lambdary/lock` pins the container backend's image digests and layer digests on first start. Commit it alongside your functions for reproducible starts across machines.

---
description: Read function output, invocation metrics and server health from a running dev server.
---

# Logs, Metrics and Health

## Logs

`lambdary dev` prints every function's output to its own terminal, with a color-coded `[name]` prefix and a Lambda-style `REPORT` line per invocation.

`lambdary logs` prints the output of a running dev server's functions from another terminal, as `[function] line`:

```sh
lambdary logs worker -f --tail 20     # follow one function; omit it for all
```

The server keeps the last 1,000 lines per function. `--tail` (default 100) picks how many to show, and `-f` keeps printing new ones until you interrupt it.

A bare function name includes the output of every published version of the function. `worker:2` selects one version, and `worker:live` the versions the alias routes to now.

`--port` picks the dev server to ask, by default `lambdary.yml`'s `port` or `8000`. Set `NO_COLOR` to turn colors off.

### `GET /_lambdary/logs`

Behind `lambdary logs`, `GET /_lambdary/logs?function=<name>&tail=<n>&follow=1` streams the same lines as NDJSON:

```sh
curl 'http://127.0.0.1:8000/_lambdary/logs?function=worker&tail=20'
```

```json
{"function":"worker","time":"2026-11-17T09:00:00.123Z","text":"processing 3 messages"}
```

| Parameter | Default | Description |
|---|---|---|
| `function` | all functions | A name, `name:version` or `name:alias`. Unknown ones answer `404`. |
| `tail` | `100` | How many buffered lines to replay, a non-negative integer. |
| `follow` | `0` | `1` keeps streaming new lines until the client disconnects. |

## Metrics

`GET /_lambdary/metrics?window=1h` returns a JSON summary of each function's invocations, errors, throttles and duration percentiles over the window (a Go duration such as `15m` or `1h`, one hour by default), in total and per version or alias:

```sh
curl 'http://127.0.0.1:8000/_lambdary/metrics?window=15m'
```

```json
{
  "window": "15m0s",
  "functions": [
    {
      "name": "notify",
      "total": {"resource": "", "invocations": 12, "errors": 1, "throttles": 0, "duration_ms": {"p50": 41.2, "p95": 120.5, "max": 133.0}},
      "resources": [
        {"resource": "notify:$LATEST", "invocations": 10, "errors": 1, "throttles": 0, "duration_ms": {"p50": 40.1, "p95": 118.9, "max": 133.0}},
        {"resource": "notify:2", "invocations": 2, "errors": 0, "throttles": 0, "duration_ms": {"p50": 45.0, "p95": 47.3, "max": 47.3}},
        {"resource": "notify:live", "invocations": 2, "errors": 0, "throttles": 0, "duration_ms": {"p50": 45.0, "p95": 47.3, "max": 47.3}}
      ]
    }
  ]
}
```

Resource values follow CloudWatch's `Resource` dimension: `<fn>:<version>`, `<fn>:$LATEST`, and `<fn>:<alias>` for invokes that went through an alias. An alias invoke counts under both its version and its alias.

### CloudWatch `GetMetricData`

The CloudWatch `GetMetricData` call works too, for the `AWS/Lambda` namespace with a `FunctionName` dimension and an optional `Resource` dimension (`<fn>:<version>`, `<fn>:<alias>` or `<fn>:$LATEST`). Current AWS SDKs send it over Smithy RPCv2 with CBOR bodies, and so does the server.

| | Supported |
|---|---|
| Metrics | `Invocations`, `Errors`, `Throttles`, `Duration` |
| Statistics | `Sum`, `SampleCount`, `Average`, `Minimum`, `Maximum`, and percentiles such as `p99` |

```sh
export AWS_ENDPOINT_URL_CLOUDWATCH=http://127.0.0.1:8000
```

Metrics are kept in memory and start over when the server restarts. An invocation whose function fails to start is not counted, as on AWS.

{% hint style="warning" %}
The `Throttles` metric reads 0 in practice. Requests to one function wait for each other instead of being throttled. See [Limitations](../reference/limitations.md#metrics).
{% endhint %}

## Health

`GET /_lambdary/health` answers `200` with each function's state, `running` or `stopped` (it starts on its next invoke), for a Compose healthcheck:

```json
{"status":"ok","functions":[{"name":"api","state":"running"},{"name":"worker","state":"stopped"}]}
```

The [Docker image](docker-compose.md) has busybox's `wget`:

```yaml
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8000/_lambdary/health"]
      interval: 5s
```

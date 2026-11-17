---
description: Every key of a function's .lambda.yml, with defaults.
---

# Function Configuration

Each function can have a `.lambda.yml` in its directory. Every key is optional, and the names mirror AWS Lambda's own configuration:

```yaml
name: api                 # default: directory name
runtime: nodejs22.x       # any AWS runtime identifier
handler: handler.handler
timeout: 30               # seconds, default 900 (AWS's maximum); 0 = no limit
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
events:                   # event sources that invoke the function locally
  - sqs:
      queue: notifications
  - schedule: rate(1 minute)
local:                    # local-development-only section
  backend: auto           # auto | container | process
  image: ""               # container image override
  command: ""             # process-backend command override
  env_file: ""            # extra environment from a dotenv-style file (explicit `environment` keys win)
```

Unknown keys are rejected, so typos surface immediately.

## Top-level keys

| Key | Default | Description |
|---|---|---|
| `name` | directory name | Function name. Also the default route, `/<name>`, and the name for the invoke API. |
| `runtime` | detected from the project file | Any AWS Lambda runtime identifier, such as `nodejs22.x`, `python3.13` or `provided.al2023`. See [discovery](../getting-started/how-it-works.md#function-discovery). |
| `handler` | none | The handler in the runtime's own notation, such as `handler.handler`. Custom runtimes (`provided.*`) don't need one. |
| `timeout` | `900` | Invocation timeout in seconds. `0` means no limit locally. Must not be negative. |
| `memory` | `128` | Memory in MB, reported to the function as `AWS_LAMBDA_FUNCTION_MEMORY_SIZE`. When set, it also caps the container on the container backend. Must not be negative. |
| `architectures` | `[x86_64]` | `x86_64` or `arm64`. Picks the container platform and the Go build target. |
| `environment` | none | Environment variables for the function. |
| `url` | see below | The function's local route. |
| `layers` | none | Up to 5 layer version ARNs or local paths. See [Layers](../guides/layers.md). |
| `events` | none | SQS queues and schedules that invoke the function. See [Queues and Schedules](../guides/queues-and-schedules.md). |
| `local` | see below | Local-development-only settings. |

### Timeout

`timeout` defaults to 900 seconds, AWS Lambda's maximum, so you don't need to set it. `timeout: 0` removes the limit locally, and a value above 900 is honored as is.

AWS only accepts 1 to 900. For `0` or a value above 900, `lambdary dev`, `lambdary list` and `lambdary invoke` print a warning that the function won't deploy as is.

{% hint style="info" %}
A slow handler that would exceed a shorter limit you configure on AWS still passes locally unless you set that limit here too.
{% endhint %}

## `url`

| Key | Default | Description |
|---|---|---|
| `url.path` | `/<name>` | The local route prefix. Must start with `/`. |
| `url.payload` | `"2.0"` | Event format. `"2.0"` sends Lambda Function URL / API Gateway HTTP API events. `"1.0"` sends API Gateway REST API events. |
| `url.invoke_mode` | `BUFFERED` | `BUFFERED` or `RESPONSE_STREAM`, as AWS's Function URL `InvokeMode`. |

`url.invoke_mode: RESPONSE_STREAM` parses a `streamifyResponse` handler's `http-integration-response` frame into its real status code, headers and cookies, instead of leaking the frame into the body. The response is still buffered end to end, so there's no time-to-first-byte benefit locally, only a correct one-shot response. `RESPONSE_STREAM` can't be combined with `url.payload: "1.0"`.

Function routes always run `$LATEST`. To invoke a version or alias, use the [invoke API](../guides/versions-and-aliases.md).

## `local`

The one section that goes beyond AWS's own configuration vocabulary:

| Key | Default | Description |
|---|---|---|
| `local.backend` | `auto` | `auto`, `container` or `process`. `container` and `process` pin this function to that backend, whatever `lambdary dev --backend` says. |
| `local.image` | AWS's base image for `runtime` | The container image to run on the container backend. |
| `local.command` | the runtime's shim, or `./bootstrap` | The command the process backend runs instead, through `sh -c`, in the function directory. |
| `local.env_file` | none | A dotenv-style file whose variables are added to the function's environment. Relative to the function directory. Keys in `environment` win. |

### `local.env_file` format

- One `KEY=VALUE` per line. Whitespace around the line and the value is trimmed.
- Blank lines and lines starting with `#` are skipped. There are no trailing comments: `KEY=VAL # note` keeps `# note` in the value.
- An `export ` prefix is stripped.
- A value may be wrapped in one pair of single or double quotes, which are removed. There are no escape sequences and no `${VAR}` interpolation.

A missing or malformed env file fails the function's start, naming the file.

## Precedence

A setting is resolved from, in order:

1. An inline entry in `lambdary.yml`'s `functions:`, when the project lists its functions.
2. The function's own `.lambda.yml`.
3. `lambdary.yml`'s `defaults:`.
4. Detection from the project file (`runtime` only), then the built-in defaults above.

Each key is taken whole from the first place that sets it. A function with its own `environment` map does not merge in `defaults.environment`; it replaces it. `events` are never inherited from `defaults:`.

## Validation

A `.lambda.yml` that fails to parse or validate does not stop the server. The function is still listed, with an `invalid manifest` warning naming the problem. These rules apply:

- `local.backend` must be empty, `auto`, `container` or `process`.
- `timeout` and `memory` must not be negative.
- `url.path` must start with `/`.
- `url.invoke_mode` must be empty, `BUFFERED` or `RESPONSE_STREAM`, and `RESPONSE_STREAM` needs `url.payload` `"2.0"`.
- `layers` holds at most 5 entries, each a layer version ARN or a local path.
- `events` entries are checked as described in [Queues and Schedules](../guides/queues-and-schedules.md#validation).

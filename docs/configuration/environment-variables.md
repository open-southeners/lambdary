---
description: Environment variables Lambdary reads, and the ones it sets for your functions.
---

# Environment Variables

## Variables Lambdary reads

| Variable | Meaning |
|---|---|
| `LAMBDARY_ROOT` | Default for `--root` on every command. An explicit `--root` still wins. |
| `LAMBDARY_NETWORK` | Docker network for function containers. Required when Lambdary runs in a container on several networks. Also works outside a container. |
| `LAMBDARY_HOST_ROOT` | Host path of the `--root` directory, when Lambdary runs in a container and can't work it out itself. |
| `LAMBDARY_HOME` | Lambdary's machine-wide state folder, `~/.lambdary` by default. Holds the process backend's emulator binary and shims. |
| `LAMBDARY_RIE_PATH` | Path to an `aws-lambda-rie` binary for the process backend to use as is, instead of the built-in or built-from-source one. |
| `NO_COLOR` | When set, to any value, function output is printed without colors. Colors are also off when output isn't a terminal. |

`LAMBDARY_NETWORK` and `LAMBDARY_HOST_ROOT` matter mostly in Docker Compose. See [Docker Compose](../guides/docker-compose.md#settings).

Layer version ARNs are fetched with your own AWS credentials, through the standard AWS SDK credential chain (environment variables, shared config and credentials files, and so on). See [Layers](../guides/layers.md).

## Variables your functions get

Each function's environment is built from, lowest priority first:

1. On the process backend only, the environment `lambdary` itself runs with.
2. The AWS variables, when [`aws.environment`](project-config.md#aws) is on.
3. `local.env_file`.
4. `environment` in `.lambda.yml` (or inherited from `defaults:`).
5. The Lambda variables below, which Lambdary always sets.

| Variable | Value |
|---|---|
| `AWS_LAMBDA_FUNCTION_NAME` | The function's name. |
| `AWS_LAMBDA_FUNCTION_VERSION` | The published version number, when a version runs. Not set for `$LATEST`. |
| `AWS_LAMBDA_FUNCTION_TIMEOUT` | The function's `timeout` in seconds. With `timeout: 0` (no limit) it is one year, so the emulator never cuts an invocation short. |
| `AWS_LAMBDA_FUNCTION_MEMORY_SIZE` | The function's `memory`, `128` when unset. |

AWS's emulator adds the rest of the Lambda runtime environment itself, such as `AWS_LAMBDA_RUNTIME_API`.

## AWS variables injected with `aws.environment`

With `aws.environment: true` in [`lambdary.yml`](project-config.md#aws), functions also get these, unless `local.env_file` or `environment` sets the same name:

| Variable | Value |
|---|---|
| `AWS_REGION`, `AWS_DEFAULT_REGION` | `aws.region`, `us-east-1` by default. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` | `lambdary`. The dev server accepts any credentials. |
| `AWS_ENDPOINT_URL` | Where the function reaches the dev server, as in the table below. |

| Where the function runs | `AWS_ENDPOINT_URL` |
|---|---|
| Process backend | `http://127.0.0.1:<port>` |
| Container backend, Lambdary in a container | `http://<Lambdary's container name>:<port>` on the shared network |
| Container backend, Lambdary on the host | `http://host.docker.internal:<port>` |

`<port>` is the dev server's port. For the last case Lambdary adds `--add-host=host.docker.internal:host-gateway` to the function's container, and warns at startup when the dev server listens on loopback only, which containers can't reach. Start it with `--host 0.0.0.0`.

`lambdary invoke` without a dev server sets the region and credentials but not `AWS_ENDPOINT_URL`, since nothing answers there. These values replace any `AWS_*` variable of your own shell on the process backend.

## Variables for AWS clients

To point the AWS CLI or an SDK at the dev server, set an endpoint override:

```sh
export AWS_ENDPOINT_URL=http://127.0.0.1:8000             # every service
export AWS_ENDPOINT_URL_LAMBDA=http://127.0.0.1:8000      # Lambda only
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:8000         # SQS only
export AWS_ENDPOINT_URL_CLOUDWATCH=http://127.0.0.1:8000  # CloudWatch only
```

See [Using AWS SDKs and the CLI](../guides/using-aws-sdks.md).

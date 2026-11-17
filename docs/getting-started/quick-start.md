---
description: Scaffold a function, run the dev server and call it over HTTP and the AWS CLI.
---

# Quick Start

## Scaffold a function

```sh
mkdir my-functions && cd my-functions
lambdary init api
```

`init` writes a handler stub and a matching `.lambda.yml` into `api/`. It scaffolds Node.js (`nodejs22.x`) by default. Two other runtimes have stubs:

```sh
lambdary init worker --runtime python3.13
lambdary init echo --runtime provided.al2023
```

| Runtime | Files written | `.lambda.yml` |
|---|---|---|
| `nodejs22.x` | `handler.mjs` | `runtime: nodejs22.x`, `handler: handler.handler`, `timeout: 30` |
| `python3.13` | `lambda_function.py` | `runtime: python3.13`, `handler: lambda_function.handler`, `timeout: 30` |
| `provided.al2023` | `bootstrap` (an executable shell script that echoes each event back) | `runtime: provided.al2023` |

Any other AWS runtime works too. `init` just has no stub for it, so write the handler and set `runtime` in `.lambda.yml` yourself.

## Run the dev server

```sh
lambdary dev
```

```
lambdary dev server listening on http://127.0.0.1:8000
backend: container (docker)

  api     /api     (nodejs22.x)
```

Call the function on its route:

```sh
curl http://127.0.0.1:8000/api
```

The request reaches your handler as a Lambda Function URL event, and the handler's response comes back as the HTTP response. See [How It Works](how-it-works.md) for the details.

## Edit and save

Edit the handler and save. `lambdary dev` hot-reloads:

- A code edit restarts only that function, on its next request.
- Adding or removing a function directory, or editing a `.lambda.yml` or `lambdary.yml`, reconfigures the server live.
- A broken edit is logged and the previous configuration keeps running, instead of taking the server down.

Function output, including a Lambda-style `REPORT` line per invocation, streams to your terminal with a color-coded `[name]` prefix. `--no-reload` turns hot reload off.

## Use the AWS CLI and SDKs

The server also exposes Lambda's real invoke endpoint, so the AWS CLI and any AWS SDK work against it unchanged:

```sh
aws lambda invoke --endpoint-url http://127.0.0.1:8000 --function-name api out.json
```

Or with plain HTTP:

```sh
curl -X POST http://127.0.0.1:8000/2015-03-31/functions/api/invocations -d '{"any":"event"}'
```

See [Using AWS SDKs and the CLI](../guides/using-aws-sdks.md) for endpoint settings and credentials.

## Invoke once without a server

`lambdary invoke` runs one invocation and prints the response. It uses the dev server if one is running, and otherwise starts the function just for that call:

```sh
lambdary invoke api
lambdary invoke api --event event.json
lambdary invoke api --event apigw --set path=/hello
```

See [Sample Events](../guides/sample-events.md) for the built-in event types.

## Next steps

- [How It Works](how-it-works.md) explains discovery, events and the two backends.
- [Function Configuration](../configuration/function-config.md) lists every `.lambda.yml` key.

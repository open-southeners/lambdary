---
description: Every lambdary command and flag.
---

# Commands

```
lambdary dev      [--port N] [--host ADDR] [--backend auto|container|process] [--eager] [--no-reload] [--no-schedules]
lambdary list                                # discovered functions, runtime, route
lambdary invoke <fn> [-e event.json|-|sqs|s3|schedule|apigw] [--set key=value] [--port N] [--backend B]
                                             # one-shot invoke (uses the dev server if running)
lambdary logs [fn] [-f] [--tail N] [--port P]  # a running dev server's function output
lambdary init <name> [--runtime <id>]        # scaffold a function
```

## Global flags

| Flag | Default | Description |
|---|---|---|
| `--root <dir>` | `$LAMBDARY_ROOT`, else `.` | The functions root folder. An optional `lambdary.yml` directly under it configures the project. |
| `-v`, `--version` | | Print the version. |
| `-h`, `--help` | | Help for any command. |

`lambdary completion <shell>` generates a shell completion script.

## `lambdary dev`

Discover the functions under `--root` and serve them all behind one local HTTP server, with hot reload.

```sh
lambdary dev
lambdary dev --port 9000 --backend process
lambdary dev --host 0.0.0.0 --eager
```

| Flag | Default | Description |
|---|---|---|
| `--port <n>` | `lambdary.yml` `port`, else `8000` | Port of the HTTP server. |
| `--host <addr>` | `127.0.0.1` | Address the server listens on. `0.0.0.0` accepts other machines or containers, with no authentication. |
| `--backend <b>` | `auto` | Default backend for functions whose own `local.backend` is `auto` or unset: `auto`, `container` or `process`. |
| `--eager` | off | Start every function's backend immediately instead of on its first invoke. |
| `--no-reload` | off | Disable hot reload; function and config edits need a restart. |
| `--no-schedules` | off | Don't fire the schedules in functions' `events`. |

The server prints its address, the backend in use, and each function with its route and runtime. When functions use SQS events, it also lists the queues and the `AWS_ENDPOINT_URL_SQS` value to use.

Hot reload is on by default: editing a function's code restarts just that function, on its next request; adding or removing a function directory or editing a `.lambda.yml` or `lambdary.yml` reconfigures the whole server. A broken edit is logged and the previous configuration keeps running.

Stop the server with `Ctrl-C` (SIGINT) or SIGTERM. It stops every running function before exiting.

## `lambdary list`

List the discovered functions as a table, with any warnings (an invalid `.lambda.yml`, an unknown runtime, route or name collisions, a timeout AWS won't accept):

```sh
lambdary list
```

```
NAME    RUNTIME     BACKEND  ROUTE    DIR
api     nodejs22.x  auto     /api     api
worker  python3.13  auto     /worker  worker
```

It has no flags of its own.

## `lambdary invoke`

Invoke one function once and print its response.

```sh
lambdary invoke api
lambdary invoke api --event event.json
lambdary invoke worker -e sqs --set body='{"id":1}' --set count=3
```

| Flag | Default | Description |
|---|---|---|
| `-e`, `--event <e>` | `{}` | A JSON event file, `-` for stdin, or a sample event type: `sqs`, `s3`, `schedule` or `apigw`. A file named like a type takes precedence. |
| `--set key=value` | | Override a field of a sample event type. Repeatable. Needs a sample event type. |
| `--port <n>` | `lambdary.yml` `port`, else `8000` | The dev server port to probe. |
| `--backend <b>` | `auto` | Backend for standalone mode, for a function whose own `local.backend` is `auto`. Ignored in server mode. |

**Server mode.** If a `lambdary dev` server is listening on `--port`, `invoke` posts the event to its invoke API and prints the response.

**Standalone mode.** If no dev server answers, `invoke` discovers the function itself, starts its backend just for this call, invokes it, prints the response and stops the backend again.

See [Sample Events](../guides/sample-events.md) for every `--set` key.

## `lambdary logs`

Print the output of a running dev server's functions, as `[function] line`.

```sh
lambdary logs                    # every function
lambdary logs worker -f --tail 20
lambdary logs notify:live        # the versions an alias routes to
```

| Flag | Default | Description |
|---|---|---|
| `-f`, `--follow` | off | Keep printing new lines until interrupted. |
| `--tail <n>` | `100` | How many past lines to show. The server keeps the last 1,000 per function. |
| `--port <n>` | `lambdary.yml` `port`, else `8000` | The dev server port. |

A bare name includes every published version of the function; `name:version` or `name:alias` selects just that version, or the versions the alias routes to.

## `lambdary init`

Scaffold a new function directory under `--root`: a handler stub and a matching `.lambda.yml`, ready for `lambdary dev`.

```sh
lambdary init api
lambdary init worker --runtime python3.13
lambdary init echo --runtime provided.al2023
```

| Flag | Default | Description |
|---|---|---|
| `--runtime <id>` | `nodejs22.x` | `nodejs22.x`, `python3.13` or `provided.al2023`. |

`init` fails if the directory already exists. Any other AWS runtime can still be set by hand in `.lambda.yml`; `init` just has no stub for it. See [Quick Start](../getting-started/quick-start.md#scaffold-a-function) for what each stub contains.

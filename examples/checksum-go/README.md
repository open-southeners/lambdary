# checksum-go

SHA-256 checksum service, built as a compiled Go binary. Where
`screenshot-node` and `thumbnail-python` lean on Lambdary's built-in Node
and Python shims, this one is a `provided.al2023` **custom runtime**:
`main.go` speaks AWS's Lambda Runtime API loop directly, with no shim in
between.

It also demonstrates a different discovery path than the other two
examples: this directory has its own `Dockerfile` and no `runtime` key in
`.lambda.yml`, so lambdary builds and runs it as a container automatically
— `docker build`, on every start, hot reload included. (A bare bind-mounted
`bootstrap` — what `lambdary init --runtime provided.al2023` scaffolds —
now runs on either backend with no Dockerfile needed, but only if it's
already an executable; Go's `bootstrap` is compiled from `main.go`, and
lambdary has no automatic build step for compiled runtimes, so a
function-owned Dockerfile that builds the binary is still the practical
choice here. See the Dockerfile's comments.)

Between the three examples, that's Lambdary's full surface: process
backend with a language shim, and container backend both with
(`thumbnail-python`, if Docker's available) and without
(`screenshot-node`, pinned to process) a shim, plus a function-owned
Dockerfile here.

## Setup

Nothing to install — Docker (or Podman/Finch/nerdctl) builds the Go binary
inside the image, so you don't need Go on your host at all for this one.

## Try it

From the `examples/` folder:

```sh
lambdary dev
curl -X POST --data 'hello lambdary' http://127.0.0.1:8000/checksum
# {"bytes":14,"sha256":"..."}
```

The first request is slower while Docker builds the image; edits to
`main.go` or the `Dockerfile` itself trigger a rebuild on the next restart.

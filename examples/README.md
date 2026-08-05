# Examples

A small playground of real Lambda functions to run against `lambdary dev`
and poke at, covering different corners of Lambdary's HTTP⇆event mapping:

| Function | Runtime | Backend | Exercises |
|---|---|---|---|
| [`screenshot-node`](screenshot-node) | `nodejs22.x` | process (pinned) | GET + query strings, outgoing binary body (PNG) |
| [`thumbnail-python`](thumbnail-python) | `python3.13` | auto | POST, incoming *and* outgoing binary body, `environment` config |
| [`checksum-go`](checksum-go) | `provided.al2023` (custom, own `Dockerfile`) | container | a language with no built-in shim, container backend, `Dockerfile`-based functions |

Each has its own README with setup and `curl` examples. Once they're all
set up, run them together:

```sh
cd examples
lambdary dev
```

```
lambdary dev server listening on http://127.0.0.1:8000

  checksum-go       /checksum    (-)
  screenshot-node   /screenshot  (nodejs22.x)
  thumbnail-python  /thumbnail   (python3.13)
```

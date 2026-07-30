# RIE darwin/arm64 build spike — findings

Unit E of the M0 plan (`plans/m0-skeleton.md`). Goal: prove or disprove that
AWS's Lambda Runtime Interface Emulator (RIE) builds from pinned upstream
source and runs correctly on darwin/arm64, since AWS only ships Linux
binaries. See DESIGN.md ("Process backend") for the recorded fallback this
spike gates.

**Verdict: the DESIGN.md fallback (in-house Runtime API server) is NOT
needed for darwin/arm64.** Upstream RIE `v1.35` builds cleanly with a plain
`go build` and runs a full invoke → Runtime API → response round trip
correctly on the host.

## Environment

- Host: darwin/arm64 (Darwin 25.5.0)
- Host Go: `go1.25.5 darwin/arm64`
- Upstream RIE pin: tag `v1.35` (commit `94ba1e9745edd515dcb5ebbf21374b80c59ca0a9`)
- Upstream `go.mod` directive: `go 1.25.7`

All work was done in a scratch directory outside the repo; no Lambdary
source was touched.

## 1. Clone

```sh
git clone --depth 1 --branch v1.35 \
  https://github.com/aws/aws-lambda-runtime-interface-emulator
```

Succeeded without issue (shallow clone, single tag). Repo layout confirms
`cmd/aws-lambda-rie` is the entrypoint package and everything else lives
under `internal/`, matching DESIGN.md's note that the module can't be
imported as a library from another module.

## 2. Build

```sh
cd aws-lambda-runtime-interface-emulator
GOTOOLCHAIN=auto go build -o aws-lambda-rie ./cmd/aws-lambda-rie
```

- Deliberately avoided upstream's `Makefile` (its targets assume a Linux
  build environment); `go build` against the `cmd/aws-lambda-rie` package
  directly was sufficient.
- The host's go1.25.5 does not satisfy the module's `go 1.25.7` directive.
  With `GOTOOLCHAIN=auto`, the toolchain manager transparently downloaded
  and used `go1.25.7 darwin/arm64` (confirmed via
  `go version -m aws-lambda-rie` → `aws-lambda-rie: go1.25.7`, and the
  fetched toolchain module under
  `$(go env GOPATH)/pkg/mod/golang.org/toolchain@v0.0.1-go1.25.7.darwin-arm64`).
  Note: this host's `go env GOTOOLCHAIN` already defaults to `auto`, so the
  explicit env var is a documentation/safety measure rather than a strict
  requirement here — but it should still be set explicitly in any pinned
  build script since a differently configured host could have
  `GOTOOLCHAIN=local`.
- No compile errors, no darwin-specific `#build` tag issues, no Linux-only
  syscalls surfaced (e.g. nothing under `cmd/aws-lambda-rie` or its
  transitive deps needed `cgo` or Linux-only packages for a plain build —
  see caveats below for what *wasn't* exercised).
- Output binary: **`aws-lambda-rie`, 12,644,146 bytes (~12 MB)**, confirmed
  `Mach-O 64-bit executable arm64` via `file`.

No workaround beyond `GOTOOLCHAIN=auto` was needed — the build succeeded on
the first attempt with the plain `go build` command above.

## 3. Smoke test

### Bootstrap script

Created a minimal custom-runtime function directory containing an
executable `bootstrap` implementing the Runtime API loop with `curl`:

```sh
#!/bin/sh
set -eu

while true; do
  HEADERS="$(mktemp)"
  EVENT_BODY=$(curl -sS -D "$HEADERS" "http://${AWS_LAMBDA_RUNTIME_API}/2018-06-01/runtime/invocation/next")
  REQUEST_ID=$(grep -Fi Lambda-Runtime-Aws-Request-Id "$HEADERS" | tr -d '\r' | cut -d: -f2 | tr -d ' ')
  rm -f "$HEADERS"

  curl -sS -X POST \
    "http://${AWS_LAMBDA_RUNTIME_API}/2018-06-01/runtime/invocation/${REQUEST_ID}/response" \
    -d "$EVENT_BODY" > /dev/null
done
```

### Launching the RIE

The RIE binary takes the bootstrap command as trailing positional args and
must be run with the function directory as its working directory (it
resolves the bootstrap path and, when no bootstrap arg is given, hardcodes
a Linux-container-style lookup under `/var/task`, `/opt/bootstrap`,
`/var/runtime/bootstrap` — irrelevant on darwin, so the bootstrap must be
passed explicitly):

```sh
cd smoketest-fn
./aws-lambda-rie --runtime-interface-emulator-address 127.0.0.1:9002 ./bootstrap
```

**Darwin-specific/local gotcha found:** the RIE also runs an *internal*
Runtime API server (the one `AWS_LAMBDA_RUNTIME_API` points the bootstrap
loop at) which defaults to port **9001**
(`internal/lambda/rapidcore/sandbox_builder.go`: `RuntimeAPIPort: 9001`).
Picking `9001` for `--runtime-interface-emulator-address` (the public
invoke port) collides with that internal default and crashes the process
at startup:

```
[PANIC] (rapid) Runtime API Server failed to listen error=listen tcp 127.0.0.1:9001: bind: address already in use
```

This is not darwin-specific behavior (it would happen on Linux too) but is
worth documenting since it's an easy port-choice trap when wiring up the
process backend: **never default the public/emulator port to 9001**, since
that's reserved internally for the Runtime API side unless
`--runtime-api-address` is also set explicitly. Re-running with
`--runtime-interface-emulator-address 127.0.0.1:9002` started cleanly.

### Invocation

```sh
curl -s -w "\nHTTP_STATUS:%{http_code}\n" -XPOST \
  http://localhost:9002/2015-03-31/functions/function/invocations \
  -d '{"ping":"pong"}'
```

Output:

```
{"ping":"pong"}
HTTP_STATUS:200
```

The event was echoed back unmodified with a `200` status, exactly as
expected from the bootstrap's next/response loop. A second invocation with
a different payload (`{"second":"invocation","n":2}`) was sent to confirm
the RIE correctly loops for subsequent invocations (not just a one-shot),
and it also echoed back correctly with `200`.

The RIE's own log output for both invocations matches Lambda's real
`REPORT`-style format:

```
START RequestId: afc36c9d-9593-4c74-9667-c6d9f50a3eb6 Version: $LATEST
... INIT START / INIT RTDONE / INIT REPORT ...
... INVOKE START(requestId: afc36c9d-...) / INVOKE RTDONE(status: success, ...) ...
END RequestId: afc36c9d-9593-4c74-9667-c6d9f50a3eb6
REPORT RequestId: afc36c9d-9593-4c74-9667-c6d9f50a3eb6	Init Duration: 0.02 ms	Duration: 304.42 ms	Billed Duration: 305 ms	Memory Size: 3008 MB	Max Memory Used: 3008 MB
START RequestId: 8cb72e73-c245-47ea-86d5-51d06a0dce8e Version: $LATEST
... INVOKE START / INVOKE RTDONE ...
END RequestId: 8cb72e73-c245-47ea-86d5-51d06a0dce8e
REPORT RequestId: 8cb72e73-c245-47ea-86d5-51d06a0dce8e	Duration: 39.90 ms	Billed Duration: 40 ms	Memory Size: 3008 MB	Max Memory Used: 3008 MB
```

The RIE process was killed (`pkill -f aws-lambda-rie`) after the test;
confirmed no lingering process afterward.

## Caveats / not exercised by this spike

- Only the `provided.al2023`-style custom-runtime path (a plain shell
  `bootstrap`) was exercised. Language-specific Runtime Interface Clients
  (`aws-lambda-ric` for Node, `awslambdaric` for Python, etc.) were not
  built or tested here — those are separate binaries/packages from
  separate repos, out of scope for this spike, and should get their own
  smoke test when the process backend (M3) is implemented.
- Only a successful/happy-path invocation was tested — error paths
  (`…/invocation/<id>/error`), timeouts, and Init Caching were not
  exercised.
- The build was a native `go build` for the host architecture only; no
  attempt was made to cross-compile for darwin/amd64 or verify Lambdary's
  eventual `go:embed`-based packaging/extraction flow — this spike only
  proves the upstream source builds and runs standalone.

## Summary

| Question | Answer |
|---|---|
| Does upstream RIE `v1.35` build on darwin/arm64? | Yes, `go build ./cmd/aws-lambda-rie` with `GOTOOLCHAIN=auto`, no Makefile, no source patches, first try. |
| Toolchain actually used | `go1.25.7 darwin/arm64` (auto-fetched; host had go1.25.5). |
| Binary size | 12,644,146 bytes (~12 MB), Mach-O arm64. |
| Does the built binary run a full invoke round trip on darwin? | Yes — confirmed with a custom-runtime `bootstrap` and two invocations via the Invoke API, correct `200` + echoed body, correct `REPORT` logging. |
| darwin-specific issues found | None in the build. One port-choice footgun at runtime (internal Runtime API default port `9001` collides with a poorly chosen `--runtime-interface-emulator-address`) — not darwin-specific, but worth noting for the process backend's port allocation logic. |
| Is the DESIGN.md in-house Runtime API server fallback needed for darwin/arm64? | **No.** |

#!/usr/bin/env python3
"""Lambdary's dependency-free Python Runtime API shim.

See plans/m3-process-path.md's Unit B "Shims, not RICs" decision and
DESIGN.md's "Process backend" section for why this exists instead of the
official awslambdaric (native components, per-function compile toolchain).

Usage: python3 bootstrap.py <handler>, where <handler> is AWS's own
"module.function" notation (e.g. "app.handler", "src.app.handler") — the
LAST "." separates the importable module path from the function name. The
module part is imported with importlib.import_module, so it must be
importable as a regular Python module from the function's own directory
(added to sys.path); a bare script that isn't import-able needs
`local.command` and the real awslambdaric instead. Only plain callables
`handler(event, context)` are supported — no generator/streaming handlers.

Runtime API loop (frozen, versioned API — see DESIGN.md's "Background"
section), mirroring bootstrap.mjs one-for-one against the same endpoints:
  GET  $AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/next
  POST .../invocation/<id>/response   on a successful handler call
  POST .../invocation/<id>/error      when the handler raises
  POST .../runtime/init/error + exit 1   when the handler fails to load

The RIE sets AWS_LAMBDA_RUNTIME_API on this process itself before spawning
it (see plans/rie-darwin-spike.md); this shim only reads it.
"""

import importlib
import json
import os
import sys
import time
import traceback
import types
import urllib.request


def post_json(url, body):
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, method="POST", headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req) as resp:
        resp.read()


def error_payload(exc):
    return {
        "errorMessage": str(exc),
        "errorType": type(exc).__name__,
        "stackTrace": traceback.format_exception(type(exc), exc, exc.__traceback__),
    }


def load_handler(spec):
    if "." not in spec:
        raise ValueError(f'invalid handler {spec!r}: want "module.function"')

    module_name, func_name = spec.rsplit(".", 1)

    sys.path.insert(0, os.getcwd())
    module = importlib.import_module(module_name)

    fn = getattr(module, func_name, None)
    if not callable(fn):
        raise ValueError(f"{module_name} has no callable {func_name!r}")

    return fn


def main():
    runtime_api = os.environ.get("AWS_LAMBDA_RUNTIME_API")
    if not runtime_api or len(sys.argv) < 2:
        print(
            "bootstrap.py: AWS_LAMBDA_RUNTIME_API env var and a handler argument are required",
            file=sys.stderr,
        )
        sys.exit(1)

    base = f"http://{runtime_api}/2018-06-01/runtime"

    try:
        handler = load_handler(sys.argv[1])
    except Exception as exc:  # noqa: BLE001 - handler loading is expected to fail arbitrarily.
        post_json(f"{base}/init/error", error_payload(exc))
        sys.exit(1)

    function_name = os.environ.get("AWS_LAMBDA_FUNCTION_NAME", "")

    while True:
        with urllib.request.urlopen(f"{base}/invocation/next") as resp:
            request_id = resp.headers.get("Lambda-Runtime-Aws-Request-Id")
            deadline_ms = resp.headers.get("Lambda-Runtime-Deadline-Ms")
            event = json.loads(resp.read() or b"null")

        deadline_ms = int(deadline_ms) if deadline_ms else None

        context = types.SimpleNamespace(
            aws_request_id=request_id,
            function_name=function_name,
            get_remaining_time_in_millis=(
                lambda deadline=deadline_ms: (deadline - int(time.time() * 1000)) if deadline else 0
            ),
        )

        try:
            result = handler(event, context)
            post_json(f"{base}/invocation/{request_id}/response", result)
        except Exception as exc:  # noqa: BLE001 - any handler exception must reach the Runtime API's error endpoint.
            post_json(f"{base}/invocation/{request_id}/error", error_payload(exc))


if __name__ == "__main__":
    main()

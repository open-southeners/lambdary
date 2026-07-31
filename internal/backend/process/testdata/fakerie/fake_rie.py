#!/usr/bin/env python3
"""Fake RIE binary for process_test.go's Start() integration test.

Stands in for the real aws-lambda-rie binary: binds the address given by
--runtime-interface-emulator-address and answers 200 to every request,
which is all process.waitReady needs to consider the instance ready. It
does NOT exec the trailing runtime command (no node/python3/bootstrap
needed to run this test) -- instead, when $FAKE_RIE_RECORD is set, it
records its own argv/cwd/environment to that path as JSON, so the Go test
can assert Start() built the right RIE argv, env, and cwd without needing
the real RIE or a host language runtime. Full RIE integration lives in
Unit C's e2e tests instead (plans/m3-process-path.md's Unit B note).
"""
import http.server
import json
import os
import socketserver
import sys


def get_addr(flag):
    idx = sys.argv.index(flag)
    return sys.argv[idx + 1]


def main():
    record_path = os.environ.get("FAKE_RIE_RECORD")
    if record_path:
        with open(record_path, "w") as f:
            json.dump({"argv": sys.argv[1:], "cwd": os.getcwd(), "env": dict(os.environ)}, f)

    addr = get_addr("--runtime-interface-emulator-address")
    host, port = addr.rsplit(":", 1)

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            self.send_response(200)
            self.end_headers()

        def do_POST(self):
            self.send_response(200)
            self.end_headers()

        def log_message(self, *args):
            pass

    with socketserver.TCPServer((host, int(port)), Handler) as httpd:
        httpd.serve_forever()


if __name__ == "__main__":
    main()

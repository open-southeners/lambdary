package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// defaultInitRuntime is the runtime lambdary init scaffolds when --runtime
// is left unset, per DESIGN.md's CLI surface ("lambdary init [runtime]").
const defaultInitRuntime = "nodejs22.x"

// initFilePerm/initDirPerm are the permissions used for scaffolded files
// and the function directory itself. initBootstrapPerm additionally sets
// the executable bit on the provided.al2023 bootstrap script, per AWS's
// custom-runtime contract (DESIGN.md's "PHP / custom runtimes" decision).
const (
	initDirPerm       = 0o755
	initFilePerm      = 0o644
	initBootstrapPerm = 0o755
)

// supportedInitRuntimes lists the --runtime values init knows how to
// scaffold, in the order shown in usage/error text. Any other AWS runtime
// id is still valid to hand-write into a .lambda.yml — init just doesn't
// have a stub for it.
var supportedInitRuntimes = []string{"nodejs22.x", "python3.13", "provided.al2023"}

func newInitCmd() *cobra.Command {
	var runtimeFlag string

	cmd := &cobra.Command{
		Use:   "init <name>",
		Short: "Scaffold a new function directory",
		Long: fmt.Sprintf(`Scaffold a new function directory under --root: a runtime-appropriate
handler stub plus a matching .lambda.yml manifest, ready for "lambdary dev".

Supported --runtime values: %s. Any other AWS Lambda runtime id can still
be set by hand in .lambda.yml — init just doesn't have a stub for it.`, strings.Join(supportedInitRuntimes, ", ")),
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd.OutOrStdout(), root, args[0], runtimeFlag)
		},
	}

	cmd.Flags().StringVar(&runtimeFlag, "runtime", defaultInitRuntime, "runtime to scaffold ("+strings.Join(supportedInitRuntimes, ", ")+")")

	return cmd
}

// runInit resolves the scan root (respecting --root and lambdary.yml, per
// resolveRoot), refuses to touch an existing <name> directory, then writes
// the runtime's scaffold and prints next steps to out.
func runInit(out io.Writer, root, name, runtimeVal string) error {
	if err := validateInitRuntime(runtimeVal); err != nil {
		return err
	}

	scanRoot, cfg, err := resolveRoot(root)
	if err != nil {
		return err
	}

	dir := filepath.Join(scanRoot, name)

	if _, statErr := os.Stat(dir); statErr == nil {
		return fmt.Errorf("init: %s already exists", dir)
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("init: %s: %w", dir, statErr)
	}

	if err := os.Mkdir(dir, initDirPerm); err != nil {
		return fmt.Errorf("init: creating %s: %w", dir, err)
	}

	if err := scaffold(dir, name, runtimeVal); err != nil {
		return err
	}

	port := resolvePort(0, cfg)

	printInitNextSteps(out, dir, name, port)

	return nil
}

// validateInitRuntime checks v against supportedInitRuntimes, returning an
// error listing them (and noting hand-written manifests aren't limited to
// that list) when v isn't one of them.
func validateInitRuntime(v string) error {
	for _, r := range supportedInitRuntimes {
		if v == r {
			return nil
		}
	}

	return fmt.Errorf("init: unsupported --runtime %q; supported: %s (any other AWS runtime id can still be set by hand in .lambda.yml)",
		v, strings.Join(supportedInitRuntimes, ", "))
}

// scaffold writes dir's runtime-specific handler stub and .lambda.yml
// manifest for name. runtimeVal is already validated by validateInitRuntime.
func scaffold(dir, name, runtimeVal string) error {
	switch runtimeVal {
	case "nodejs22.x":
		return scaffoldNode(dir, name)
	case "python3.13":
		return scaffoldPython(dir, name)
	case "provided.al2023":
		return scaffoldProvided(dir)
	default:
		return fmt.Errorf("init: unsupported --runtime %q", runtimeVal)
	}
}

// scaffoldNode writes handler.mjs (an "export const handler" ESM stub the
// Node shim's "file.export" handler resolution loads directly, see
// internal/backend/process/shims/bootstrap.mjs) and a .lambda.yml pointing
// handler at it.
func scaffoldNode(dir, name string) error {
	handler := fmt.Sprintf("export const handler = async (event) => ({ message: %q });\n", "Hello from "+name+"!")
	if err := writeInitFile(dir, "handler.mjs", handler, initFilePerm); err != nil {
		return err
	}

	manifest := "runtime: nodejs22.x\nhandler: handler.handler\ntimeout: 30\n"

	return writeInitFile(dir, ".lambda.yml", manifest, initFilePerm)
}

// scaffoldPython writes lambda_function.py (a "def handler(event, context)"
// stub the Python shim's "module.function" handler resolution imports
// directly, see internal/backend/process/shims/bootstrap.py) and a
// .lambda.yml pointing handler at it.
func scaffoldPython(dir, name string) error {
	handler := fmt.Sprintf("def handler(event, context):\n    return {\"message\": %q}\n", "Hello from "+name+"!")
	if err := writeInitFile(dir, "lambda_function.py", handler, initFilePerm); err != nil {
		return err
	}

	manifest := "runtime: python3.13\nhandler: lambda_function.handler\ntimeout: 30\n"

	return writeInitFile(dir, ".lambda.yml", manifest, initFilePerm)
}

// providedBootstrap is a minimal, WORKING custom-runtime (provided.al2023)
// bootstrap script:
// the same curl-based Runtime API next/response loop verified against the
// real RIE on darwin in plans/rie-darwin-spike.md, echoing each event back
// unmodified. The leading comment block explains the Runtime API contract
// (DESIGN.md's "Background: how Lambda invocation works locally") so a
// working echo beats an unexplained exit-1 placeholder as a starting point.
const providedBootstrap = `#!/bin/sh
# Lambdary custom-runtime (provided.al2023) bootstrap.
#
# AWS's custom-runtime contract, a.k.a. the Runtime API: a function's own
# "bootstrap" executable drives a small long-poll HTTP loop against
# $AWS_LAMBDA_RUNTIME_API (see DESIGN.md's "Background" section):
#
#   GET  http://$AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/next
#     blocks until an event arrives; the "Lambda-Runtime-Aws-Request-Id"
#     response header carries the invocation's request ID, the body is the
#     raw JSON event.
#   POST http://$AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/<id>/response
#     the handler's result (raw bytes, usually JSON), on success.
#   POST .../invocation/<id>/error   instead of /response, on failure.
#
# This stub just echoes every event straight back below — replace the body
# of the loop with a real handler in any language/binary you like, as long
# as it keeps speaking this same contract.
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
`

// scaffoldProvided writes an executable bootstrap sh stub (see
// providedBootstrap) and a .lambda.yml naming only the runtime — custom
// runtimes have no AWS "handler" notation, per DESIGN.md's ".lambda.yml
// spec" (handler is meaningless without a language-specific shim to parse
// it).
func scaffoldProvided(dir string) error {
	if err := writeInitFile(dir, "bootstrap", providedBootstrap, initBootstrapPerm); err != nil {
		return err
	}

	manifest := "runtime: provided.al2023\n"

	return writeInitFile(dir, ".lambda.yml", manifest, initFilePerm)
}

// writeInitFile writes content to name inside dir with the given
// permissions, wrapping any error with the destination path.
func writeInitFile(dir, name, content string, perm os.FileMode) error {
	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		return fmt.Errorf("init: writing %s: %w", path, err)
	}

	return nil
}

// printInitNextSteps prints the scaffolded directory, a "lambdary dev"
// hint, and the function's local URL (once dev is running) to out.
func printInitNextSteps(out io.Writer, dir, name string, port int) {
	fmt.Fprintf(out, "created %s\n\n", dir)
	fmt.Fprintf(out, "next steps:\n")
	fmt.Fprintf(out, "  lambdary dev\n")
	fmt.Fprintf(out, "  http://127.0.0.1:%d/%s\n", port, name)
}

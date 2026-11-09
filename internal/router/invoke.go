package router

import (
	"context"
	"fmt"
	"strings"
)

// InvokeResult is the outcome of an in-process Invoke.
type InvokeResult struct {
	// Payload is the function's response, or its error envelope.
	Payload []byte
	// ExecutedVersion is the version that ran.
	ExecutedVersion string
	// FunctionError reports that the handler failed, as AWS's Invoke
	// signals with X-Amz-Function-Error.
	FunctionError bool
}

// Invoke runs one synchronous invocation of the function ref names (a name
// or ARN, optionally :qualified) with payload, through the same instance,
// lock, version and retry handling as the invoke API. It serves in-process
// callers such as event source mappings and schedules.
func (rt *Router) Invoke(ctx context.Context, ref string, payload []byte) (InvokeResult, error) {
	name, qualifier := parseFunctionRef(ref)

	switch fns := rt.byName[name]; len(fns) {
	case 0:
		return InvokeResult{}, fmt.Errorf("function not found: %s", name)
	case 1:
	default:
		dirs := make([]string, len(fns))
		for i, fn := range fns {
			dirs[i] = fn.Dir
		}

		return InvokeResult{}, fmt.Errorf("function name %q is used by multiple functions: %s", name, strings.Join(dirs, ", "))
	}

	target, executed, err := rt.resolveTarget(name, qualifier, "")
	if err != nil {
		return InvokeResult{}, err
	}

	status, body, _, err := rt.invoke(ctx, target, payload)
	if err != nil {
		return InvokeResult{}, err
	}

	return InvokeResult{Payload: body, ExecutedVersion: executed, FunctionError: isFunctionError(status, body)}, nil
}

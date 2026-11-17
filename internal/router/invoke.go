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

	target, executed, err := rt.resolveRef(name, qualifier, "")
	if err != nil {
		return InvokeResult{}, err
	}

	status, body, _, err := rt.invoke(ctx, target, payload, rt.invocationMeta(name, executed, qualifier, ""))
	if err != nil {
		return InvokeResult{}, err
	}

	return InvokeResult{Payload: body, ExecutedVersion: executed, FunctionError: isFunctionError(status, body)}, nil
}

// resolveRef checks that name is exactly one discovered function and resolves
// its qualifier as resolveTarget does, for the in-process invoke paths.
func (rt *Router) resolveRef(name, refQualifier, queryQualifier string) (target, executed string, err error) {
	switch fns := rt.byName[name]; len(fns) {
	case 0:
		return "", "", fmt.Errorf("function not found: %s", name)
	case 1:
	default:
		dirs := make([]string, len(fns))
		for i, fn := range fns {
			dirs[i] = fn.Dir
		}

		return "", "", fmt.Errorf("function name %q is used by multiple functions: %s", name, strings.Join(dirs, ", "))
	}

	return rt.resolveTarget(name, refQualifier, queryQualifier)
}

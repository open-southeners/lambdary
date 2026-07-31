// Fixture handler for shim_test.go's TestNodeShim — exercises the happy
// path and the exception path against a stub Runtime API.
export async function handler(event, context) {
  return { echoed: event, requestId: context.awsRequestId, functionName: context.functionName };
}

export async function throwing() {
  throw new Error('boom');
}

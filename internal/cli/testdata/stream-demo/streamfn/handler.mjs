// A minimal RESPONSE_STREAM handler: streamifyResponse and
// HttpResponseStream.from() are provided by the nodejs22.x runtime image's
// own runtime interface client (see plans/response-streaming.md's "What was
// measured" section) — nothing here is Lambdary-specific. It sets a
// non-default status code, its own Content-Type (overriding what a
// buffered handler would default to), and a custom header, then writes its
// body in two chunks to prove the frame's body — not just its prelude — is
// what a streaming caller cares about.
export const handler = awslambda.streamifyResponse(async (event, responseStream, context) => {
  const metadata = {
    statusCode: 202,
    headers: {
      "Content-Type": "text/plain",
      "X-Stream-Probe": "yes",
    },
  };

  responseStream = awslambda.HttpResponseStream.from(responseStream, metadata);

  responseStream.write("chunk-1;");
  responseStream.write("chunk-2;");
  responseStream.end();
});

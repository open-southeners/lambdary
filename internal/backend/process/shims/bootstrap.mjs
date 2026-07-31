#!/usr/bin/env node
// Lambdary's dependency-free Node Runtime API shim — see
// plans/m3-process-path.md's Unit B "Shims, not RICs" decision and
// DESIGN.md's "Process backend" section for why this exists instead of the
// official aws-lambda-ric (native C/C++ components, per-function compile
// toolchain).
//
// Usage: node bootstrap.mjs <handler>, where <handler> is AWS's own
// "file.export" notation (e.g. "index.handler", "src/app.handler"). The
// export name is everything after the LAST "." in <handler> — AWS's own
// convention, since the file part may itself contain dots or slashes.
//
// Handler file resolution is relative to the process's current working
// directory, which the RIE always sets to the function's own directory
// (see plans/rie-darwin-spike.md): the first of "<file>.mjs", "<file>.js",
// "<file>/index.js" that exists wins. Loaded via a real ESM dynamic
// import(), so a plain ".js" file is loaded as CommonJS or ESM exactly as
// Node's own resolution algorithm (package.json "type") would decide.
//
// Supported handlers: `export function name(event, context)` /
// `export async function name(event, context)` (or the CommonJS
// equivalent, `exports.name = ...`, via Node's own CJS/ESM interop — best
// effort, since Node only statically detects simple `exports.x = ...`
// shapes; anything more dynamic needs `local.command` and the real RIC
// instead). Only async functions and functions returning a plain value are
// supported — no `(event, context, callback)` callback-style handlers.
//
// Runtime API loop (frozen, versioned API — see DESIGN.md's "Background"
// section):
//   GET  $AWS_LAMBDA_RUNTIME_API/2018-06-01/runtime/invocation/next
//   POST .../invocation/<id>/response   on a successful handler call
//   POST .../invocation/<id>/error      when the handler throws
//   POST .../runtime/init/error + exit 1   when the handler fails to load
//
// The RIE sets AWS_LAMBDA_RUNTIME_API on this process itself before
// spawning it (see plans/rie-darwin-spike.md); this shim only reads it.

import { existsSync } from 'node:fs';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const runtimeApi = process.env.AWS_LAMBDA_RUNTIME_API;
const handlerSpec = process.argv[2];

if (!runtimeApi || !handlerSpec) {
  console.error('bootstrap.mjs: AWS_LAMBDA_RUNTIME_API env var and a handler argument are required');
  process.exit(1);
}

const base = `http://${runtimeApi}/2018-06-01/runtime`;

async function loadHandler(spec) {
  const lastDot = spec.lastIndexOf('.');
  if (lastDot <= 0 || lastDot === spec.length - 1) {
    throw new Error(`invalid handler ${JSON.stringify(spec)}: want "file.export"`);
  }

  const file = spec.slice(0, lastDot);
  const exportName = spec.slice(lastDot + 1);

  const candidates = [`${file}.mjs`, `${file}.js`, path.join(file, 'index.js')];
  const found = candidates.find((candidate) => existsSync(candidate));
  if (!found) {
    throw new Error(`no handler file found for ${JSON.stringify(spec)}, tried: ${candidates.join(', ')}`);
  }

  const mod = await import(pathToFileURL(path.resolve(found)).href);
  const fn = mod[exportName] ?? mod.default?.[exportName];
  if (typeof fn !== 'function') {
    throw new Error(`${found} has no exported function ${JSON.stringify(exportName)}`);
  }

  return fn;
}

async function postJSON(url, body) {
  await fetch(url, { method: 'POST', body: JSON.stringify(body) });
}

function errorPayload(err) {
  const e = err instanceof Error ? err : new Error(String(err));

  return {
    errorMessage: e.message,
    errorType: e.name || 'Error',
    stackTrace: (e.stack || '').split('\n'),
  };
}

let handlerFn;
try {
  handlerFn = await loadHandler(handlerSpec);
} catch (err) {
  await postJSON(`${base}/init/error`, errorPayload(err));
  process.exit(1);
}

const functionName = process.env.AWS_LAMBDA_FUNCTION_NAME || '';

for (;;) {
  const next = await fetch(`${base}/invocation/next`);
  const requestId = next.headers.get('lambda-runtime-aws-request-id');
  const deadlineMs = Number(next.headers.get('lambda-runtime-deadline-ms'));
  const event = await next.json();

  const context = {
    awsRequestId: requestId,
    functionName,
    getRemainingTimeInMillis: () => (Number.isFinite(deadlineMs) ? deadlineMs - Date.now() : 0),
  };

  try {
    const result = await handlerFn(event, context);
    await postJSON(`${base}/invocation/${requestId}/response`, result === undefined ? null : result);
  } catch (err) {
    await postJSON(`${base}/invocation/${requestId}/error`, errorPayload(err));
  }
}

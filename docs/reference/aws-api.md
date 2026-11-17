---
description: Every AWS Lambda, SQS and CloudWatch call the dev server answers, and Lambdary's own endpoints.
---

# AWS API

`lambdary dev` answers a subset of the Lambda, SQS and CloudWatch APIs on its one port, plus a few endpoints of its own under `/_lambdary/`. See [Using AWS SDKs and the CLI](../guides/using-aws-sdks.md) for how to point clients at it.

## Lambda

| Call | Request | Notes |
|---|---|---|
| `Invoke` | `POST /2015-03-31/functions/<fn>/invocations` | The body is the raw event; an empty body reaches the handler as `{}`. See below. |
| `ListFunctions` | `GET /2015-03-31/functions` | `$LATEST` of each function by name. `FunctionVersion=ALL` adds every published version. Paged with `MaxItems` (default 50, at most 10000) and `Marker`. A function name shared by several functions is left out. |
| `GetFunction` | `GET /2015-03-31/functions/<fn>` | The configuration, plus a `Code` block whose location is empty. |
| `GetFunctionConfiguration` | `GET /2015-03-31/functions/<fn>/configuration` | |
| `PublishVersion` | `POST /2015-03-31/functions/<fn>/versions` | Takes `Description`, and `CodeSha256` and `RevisionId` preconditions. Returns the latest version when nothing changed. |
| `ListVersionsByFunction` | `GET /2015-03-31/functions/<fn>/versions` | `$LATEST` first, then each version. Paged like `ListFunctions`. |
| `CreateAlias` | `POST /2015-03-31/functions/<fn>/aliases` | `RoutingConfig` with one additional version. |
| `GetAlias` | `GET /2015-03-31/functions/<fn>/aliases/<alias>` | |
| `UpdateAlias` | `PUT /2015-03-31/functions/<fn>/aliases/<alias>` | Fields left out are kept. Honors `RevisionId`. |

`<fn>` can be a function name, a full or partial ARN, and can carry a `:qualifier` (a version number, an alias or `$LATEST`). `GetFunction`, `GetFunctionConfiguration` and `Invoke` also take a `Qualifier` query parameter; a qualifier in both places must match. For an alias, the configuration calls describe the alias's primary version.

Any other request under `/2015-03-31/` answers `501` `UnsupportedOperation`, so an SDK reports what Lambdary lacks rather than a generic not-found.

See [Versions and Aliases](../guides/versions-and-aliases.md) for how versions and aliases behave.

### Invoke

| Header | Direction | Meaning |
|---|---|---|
| `X-Amz-Invocation-Type` | request | `RequestResponse` (default), `Event` (answers `202`, runs in the background with retries) or `DryRun` (answers `204`, runs nothing). Anything else is a `400`. |
| `X-Amz-Executed-Version` | response | The version that ran, `$LATEST` or a number. |
| `X-Amz-Function-Error` | response | `Unhandled` when the handler failed. The status is still `200`, as on AWS. |

| Situation | Status | Error |
|---|---|---|
| Unknown function | `404` | `ResourceNotFoundException` |
| Unknown version or alias | `404` | `ResourceNotFoundException` |
| Function name shared by several functions | `409` | names the competing directories |
| The function's container or process failed to start | `502` | `ServiceException` |
| The invocation timed out | `504` | |

See [Asynchronous Invokes](../guides/async-invokes.md).

### Control-plane errors

| Status | Error |
|---|---|
| `400` | `InvalidParameterValueException`, `InvalidRequestContentException` |
| `404` | `ResourceNotFoundException` |
| `409` | `ResourceConflictException`, such as an alias that already exists |
| `412` | `PreconditionFailedException`, for a `RevisionId` or `CodeSha256` that doesn't match |
| `501` | `UnsupportedOperation` |

The error type is also sent in the `x-amzn-ErrorType` header, which AWS SDKs read.

## SQS

SQS calls are `POST` requests whose `X-Amz-Target` header names the operation, in the AWS JSON 1.0 protocol current SDKs use.

| Call | Notes |
|---|---|
| `CreateQueue` | Attributes as below. FIFO queues are rejected. |
| `GetQueueUrl` | |
| `ListQueues` | Paged with `MaxResults` and `NextToken`. |
| `DeleteQueue` | |
| `PurgeQueue` | |
| `GetQueueAttributes` | `All` or named attributes, including `QueueArn` and the `ApproximateNumberOfMessages*` counts. |
| `SetQueueAttributes` | Attributes as below. |
| `SendMessage`, `SendMessageBatch` | Message attributes and per-message `DelaySeconds`. A batch takes at most 10 entries. |
| `ReceiveMessage` | Up to 10 messages, long polling with `WaitTimeSeconds` up to 20, per-call `VisibilityTimeout`, system and message attribute selection. |
| `DeleteMessage`, `DeleteMessageBatch` | |
| `ChangeMessageVisibility`, `ChangeMessageVisibilityBatch` | |

Queue attributes Lambdary applies: `VisibilityTimeout`, `DelaySeconds`, `MessageRetentionPeriod` (default 4 days), `ReceiveMessageWaitTimeSeconds` and `RedrivePolicy`. `MaximumMessageSize`, `Policy`, `KmsMasterKeyId`, `KmsDataKeyReusePeriodSeconds`, `SqsManagedSseEnabled` and `RedriveAllowPolicy` are accepted, so deploy-shaped calls work, but not enforced. Any other attribute is rejected with `InvalidAttributeName`.

Any other SQS operation answers `400` `UnsupportedOperation`. SQS's older query protocol (`Action=SendMessage` form posts) is not served.

See [Queues and Schedules](../guides/queues-and-schedules.md).

## CloudWatch

| Call | Notes |
|---|---|
| `GetMetricData` | `POST /service/GraniteServiceVersion20100801/operation/GetMetricData`, Smithy RPCv2 CBOR. |

Only the `AWS/Lambda` namespace is served, with a required `FunctionName` dimension and an optional `Resource` dimension (`<fn>:<version>`, `<fn>:<alias>` or `<fn>:$LATEST`). The metrics are `Invocations`, `Errors`, `Throttles` and `Duration`, and the statistics `Sum`, `SampleCount`, `Average`, `Minimum`, `Maximum` and percentiles such as `p99`. Other operations, namespaces or dimensions answer `400` `InvalidParameterValueException`.

See [Logs, Metrics and Health](../guides/logs-metrics-and-health.md#cloudwatch-getmetricdata).

## Lambdary endpoints

| Endpoint | Description |
|---|---|
| `GET /` | JSON list of every function with its name, route, runtime and backend. |
| `ANY /<route>/...` | A function's route, as a Function URL event. See [How It Works](../getting-started/how-it-works.md#function-routes). |
| `GET /_lambdary/health` | `200` with each function's state, `running` or `stopped`. |
| `GET /_lambdary/metrics?window=1h` | Invocations, errors, throttles and duration percentiles per function, version and alias. |
| `GET /_lambdary/logs?function=<name>&tail=<n>&follow=1` | Buffered and live function output as NDJSON. |

See [Logs, Metrics and Health](../guides/logs-metrics-and-health.md) for the response shapes.

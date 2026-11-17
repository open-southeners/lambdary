---
description: Asynchronous Event invokes, their retries, and DryRun.
---

# Asynchronous Invokes

`Invoke` with `X-Amz-Invocation-Type: Event` answers `202` at once and runs the function in the background:

```sh
aws lambda invoke --endpoint-url http://127.0.0.1:8000 \
  --function-name worker --invocation-type Event \
  --cli-binary-format raw-in-base64-out --payload '{"id":1}' out.json
```

The `202` response carries `X-Amz-Executed-Version`, like a synchronous invoke.

## Retries

If the function fails, Lambdary retries it twice, after 1 s and after 2 s. AWS waits about 1 and 2 minutes; Lambdary shortens the waits so a failing handler is seen to fail for good within a few seconds of local work.

Schedules invoke the same way, so a failed scheduled invocation is retried too. SQS deliveries are synchronous, as on AWS: a failed batch comes back through the queue's visibility timeout instead.

## DryRun

`X-Amz-Invocation-Type: DryRun` only validates the function and answers `204`. Nothing runs.

## Invocation types

| `X-Amz-Invocation-Type` | Response |
|---|---|
| none or `RequestResponse` | The function's result, synchronously |
| `Event` | `202`, then the invocation runs in the background |
| `DryRun` | `204`, nothing runs |
| anything else | `400` `InvalidParameterValueException` |

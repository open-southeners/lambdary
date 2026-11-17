---
description: Run SQS event source mappings and EventBridge schedules locally.
---

# Queues and Schedules

Functions can declare the event sources that invoke them on AWS, and `lambdary dev` runs them locally:

```yaml
# .lambda.yml, or inline in a lambdary.yml functions entry
events:
  - sqs:
      queue: notifications          # created when the server starts
      batch_size: 10                # 1-10, or up to 10000 with batch_window
      batch_window: 0               # seconds to gather a fuller batch
      function_response_types: [ReportBatchItemFailures]
      visibility_timeout: 30        # queue settings, applied to the queue
      dlq: notifications-dlq
      max_receive_count: 5
  - schedule: rate(1 minute)        # or cron(0 9 ? * MON-FRI *), at(2026-12-01T09:00:00)
  - schedule:
      expression: cron(0 9 * * ? *)
      timezone: Europe/Madrid       # default UTC
      input: '{"kind":"digest"}'    # sent instead of the Scheduled Event
      enabled: false
```

Each `events` entry sets exactly one of `sqs` or `schedule`. Events are never inherited from `lambdary.yml`'s `defaults:`.

## SQS queues

The dev server answers SQS's API on its own port, so producers use the real AWS SDK or CLI with `AWS_ENDPOINT_URL_SQS=http://127.0.0.1:8000` (or `http://lambdary:8000` from [Compose](docker-compose.md)):

```sh
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:8000
aws sqs send-message --queue-url http://127.0.0.1:8000/000000000000/notifications --message-body '{"id":1}'
```

It supports `CreateQueue`, `GetQueueUrl`, `ListQueues`, `DeleteQueue`, `PurgeQueue`, `Get/SetQueueAttributes`, `SendMessage[Batch]`, `ReceiveMessage` with long polling, `DeleteMessage[Batch]` and `ChangeMessageVisibility[Batch]`. See [AWS API](../reference/aws-api.md#sqs) for the details.

Every queue an `sqs` event names is created when the server starts, and so is its `dlq`. Queues only producers use can be declared in `lambdary.yml`:

```yaml
queues:
  probe-results: {visibility_timeout: 60}
  deploy-requests: {dlq: deploy-requests-dlq, max_receive_count: 5}
```

When a queue is declared in several places with different settings, the first one is kept and the server warns.

Local queues are standard queues held in memory: they survive a hot reload but not a restart.

### `sqs` event keys

| Key | Default | Description |
|---|---|---|
| `queue` | required | The queue to poll. |
| `batch_size` | `10` | Most messages per invocation, 1 to 10000. Above 10 needs a `batch_window`, as on AWS. |
| `batch_window` | `0` | Seconds to keep gathering messages for a fuller batch, 0 to 300. |
| `function_response_types` | none | Only `ReportBatchItemFailures`. |
| `qualifier` | `$LATEST` | A version or alias to invoke instead. |
| `enabled` | `true` | `false` keeps the mapping from polling. |
| `visibility_timeout` | `30` | Queue setting: seconds a received message stays hidden, 0 to 43200. |
| `delay_seconds` | `0` | Queue setting: seconds every new message is delayed, 0 to 900. |
| `dlq` | none | Queue setting: dead-letter queue name. Needs `max_receive_count`. |
| `max_receive_count` | none | Queue setting: receives after which a message moves to `dlq`, 1 to 1000. |

The queue settings apply to the queue the event names.

### Delivery

An `sqs` event polls its queue and invokes the function with real SQS event JSON, as an event source mapping does:

- Messages the function handled are deleted.
- After a function error, the whole batch comes back after the visibility timeout.
- With `ReportBatchItemFailures`, only the IDs in `batchItemFailures` come back.
- A message received more than `max_receive_count` times moves to the `dlq`.

## Schedules

A schedule sends EventBridge's `Scheduled Event` each time its expression fires, in AWS's syntax:

| Expression | Example |
|---|---|
| `rate(value unit)` | `rate(1 minute)`, `rate(5 minutes)`, `rate(1 hour)`, `rate(2 days)` |
| `cron(minutes hours day-of-month month day-of-week year)` | `cron(0 9 ? * MON-FRI *)` |
| `at(yyyy-mm-ddThh:mm:ss)` | `at(2026-12-01T09:00:00)`, fires once |

`rate()` takes `minute(s)`, `hour(s)` or `day(s)`, singular for 1 and plural otherwise, as AWS requires. `cron()` uses AWS's six fields: day-of-week counts 1 (`SUN`) to 7 (`SAT`), one of the two day fields must be `?`, and names such as `JAN` or `MON-FRI` work.

A schedule can be written as the expression alone, or as a map:

| Key | Default | Description |
|---|---|---|
| `expression` | required | The `rate()`, `cron()` or `at()` expression. |
| `timezone` | `UTC` | An IANA time zone for `cron()` and `at()`, such as `Europe/Madrid`. |
| `input` | the Scheduled Event | JSON sent instead of the Scheduled Event. Must be valid JSON. |
| `qualifier` | `$LATEST` | A version or alias to invoke instead. |
| `enabled` | `true` | `false` keeps the schedule from firing. |

Schedules fire only while the server runs. A fire missed while the previous invocation was still running is skipped, not queued. Schedules invoke [asynchronously](async-invokes.md), as EventBridge does, so a failed scheduled invocation is retried.

`lambdary dev --no-schedules` turns every schedule off.

To try a handler with a Scheduled Event without waiting for it, use `lambdary invoke <fn> --event schedule`. See [Sample Events](sample-events.md).

## Validation

An invalid `events` entry makes the function's `.lambda.yml` invalid:

- Each entry sets exactly one of `sqs` or `schedule`.
- Queue names are 1 to 80 letters, digits, `-` or `_`. FIFO queues are not supported.
- `dlq` and `max_receive_count` go together, and a queue can't be its own `dlq`.
- The `cron()` modifiers `L`, `W` and `#` are not supported locally.

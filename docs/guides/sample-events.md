---
description: Build SQS, S3, schedule and API Gateway sample events for lambdary invoke.
---

# Sample Events

`lambdary invoke <fn> --event <type>` builds a sample event, so you can try a handler without writing the payload. The types are `sqs`, `s3`, `schedule` and `apigw` (an API Gateway HTTP API event, payload 2.0). A file named like a type takes precedence.

`--set key=value`, repeatable, overrides a field:

```sh
lambdary invoke worker --event sqs --set body='{"id":1}' --set count=3
lambdary invoke thumbnail --event s3 --set bucket=uploads --set key=cat.png
```

`-e` is short for `--event`. `--set` needs a sample event type; it can't change a JSON file.

## Keys

| Type | Keys |
|---|---|
| `sqs` | `body`, `queue`, `count`, `attributes.<k>` |
| `s3` | `bucket`, `key`, `size`, `event` (default `ObjectCreated:Put`) |
| `schedule` | `rule` (or `schedule`), `time` (RFC 3339), `detail` (a JSON object) |
| `apigw` | `method`, `path`, `body`, `query`, `header.<k>` |

An unknown key is rejected, naming the valid ones.

### `sqs`

| Key | Default | Description |
|---|---|---|
| `body` | `Hello from Lambdary` | Every record's message body. |
| `queue` | `my-queue` | Queue name, used in `eventSourceARN`. |
| `count` | `1` | Number of records, a positive integer. |
| `attributes.<k>` | none | Sets or overrides the system attribute `<k>`, such as `attributes.ApproximateReceiveCount=3`. |

### `s3`

| Key | Default | Description |
|---|---|---|
| `bucket` | `my-bucket` | Bucket name. |
| `key` | `path/to/object.txt` | Object key. It is URL-encoded in the event, as S3 notifications do. |
| `size` | `1024` | Object size in bytes. |
| `event` | `ObjectCreated:Put` | The `eventName`. An `s3:` prefix is dropped. |

### `schedule`

| Key | Default | Description |
|---|---|---|
| `rule` | `my-schedule` | The rule in `resources`. `schedule` is an alias for it. |
| `time` | a fixed timestamp | Event time, in RFC 3339. |
| `detail` | `{}` | The `detail` object, as JSON. |

### `apigw`

| Key | Default | Description |
|---|---|---|
| `method` | `GET` | HTTP method. |
| `path` | `/` | Request path. |
| `body` | none | Request body. |
| `query` | none | Query string, such as `page=2&sort=name`. A leading `?` is dropped. |
| `header.<k>` | none | Sets the header `<k>`, lower-cased. |

## Deterministic output

The output is the same every time: region `us-east-1`, account `000000000000` and fixed ids and timestamps, unless a key overrides them.

## Event files and stdin

`--event` also takes a path to a JSON file, or `-` for stdin. Without `--event` the event is `{}`.

```sh
lambdary invoke api --event event.json
echo '{"hello":"world"}' | lambdary invoke api --event -
```

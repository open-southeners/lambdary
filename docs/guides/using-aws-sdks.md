---
description: Point the AWS CLI and AWS SDKs at the dev server for Lambda, SQS and CloudWatch calls.
---

# Using AWS SDKs and the CLI

The dev server answers Lambda, SQS and CloudWatch calls on its one port, so the AWS CLI and any AWS SDK work against it with an endpoint override. See [AWS API](../reference/aws-api.md) for every supported call.

## Endpoints

Set one endpoint for every service, or one per service:

```sh
export AWS_ENDPOINT_URL=http://127.0.0.1:8000             # every service
export AWS_ENDPOINT_URL_LAMBDA=http://127.0.0.1:8000      # Lambda only
export AWS_ENDPOINT_URL_SQS=http://127.0.0.1:8000         # SQS only
export AWS_ENDPOINT_URL_CLOUDWATCH=http://127.0.0.1:8000  # CloudWatch only
```

The per-service variables are safer when the same process also talks to real AWS services, such as S3 or DynamoDB, that Lambdary doesn't serve. From another Compose service, use `http://lambdary:8000`; see [Docker Compose](docker-compose.md).

The AWS CLI also takes `--endpoint-url` on any command:

```sh
aws lambda invoke --endpoint-url http://127.0.0.1:8000 --function-name api out.json
```

## Credentials and region

The dev server doesn't check signatures, but AWS clients refuse to send unsigned requests. Any credentials and region work:

```sh
export AWS_ACCESS_KEY_ID=local
export AWS_SECRET_ACCESS_KEY=local
export AWS_REGION=us-east-1
```

Whatever region you sign for, ARNs in responses use region `us-east-1` and account `000000000000`. See [Limitations](../reference/limitations.md#arns-and-region).

## CLI examples

```sh
export AWS_ENDPOINT_URL=http://127.0.0.1:8000

# Lambda
aws lambda list-functions
aws lambda get-function-configuration --function-name api
aws lambda invoke --function-name api --cli-binary-format raw-in-base64-out --payload '{"id":1}' out.json
aws lambda invoke --function-name worker --invocation-type Event out.json

# SQS
aws sqs list-queues
aws sqs send-message --queue-url http://127.0.0.1:8000/000000000000/notifications --message-body '{"id":1}'
aws sqs receive-message --queue-url http://127.0.0.1:8000/000000000000/notifications --wait-time-seconds 20
```

Queue URLs have the form `http://<host>/000000000000/<queue>`. When a request names a queue URL, only the queue name at its end matters, so a URL written for AWS (`https://sqs.<region>.amazonaws.com/<account>/<queue>`) works too.

## SDK examples

{% tabs %}
{% tab title="JavaScript" %}
```js
import { LambdaClient, InvokeCommand } from '@aws-sdk/client-lambda';
import { SQSClient, SendMessageCommand } from '@aws-sdk/client-sqs';

// Picks up AWS_ENDPOINT_URL_LAMBDA / AWS_ENDPOINT_URL_SQS, or pass endpoint explicitly.
const lambda = new LambdaClient({ endpoint: 'http://127.0.0.1:8000' });
const sqs = new SQSClient({ endpoint: 'http://127.0.0.1:8000' });

const result = await lambda.send(new InvokeCommand({
  FunctionName: 'api',
  Payload: JSON.stringify({ id: 1 }),
}));
console.log(new TextDecoder().decode(result.Payload));

await sqs.send(new SendMessageCommand({
  QueueUrl: 'http://127.0.0.1:8000/000000000000/notifications',
  MessageBody: JSON.stringify({ id: 1 }),
}));
```
{% endtab %}

{% tab title="Python" %}
```python
import json
import boto3

# Picks up AWS_ENDPOINT_URL_LAMBDA / AWS_ENDPOINT_URL_SQS, or pass endpoint_url explicitly.
lam = boto3.client("lambda", endpoint_url="http://127.0.0.1:8000")
sqs = boto3.client("sqs", endpoint_url="http://127.0.0.1:8000")

result = lam.invoke(FunctionName="api", Payload=json.dumps({"id": 1}))
print(result["Payload"].read())

sqs.send_message(
    QueueUrl="http://127.0.0.1:8000/000000000000/notifications",
    MessageBody=json.dumps({"id": 1}),
)
```
{% endtab %}

{% tab title="Go" %}
```go
cfg, err := config.LoadDefaultConfig(ctx)
if err != nil {
	return err
}

client := lambda.NewFromConfig(cfg, func(o *lambda.Options) {
	o.BaseEndpoint = aws.String("http://127.0.0.1:8000")
})

out, err := client.Invoke(ctx, &lambda.InvokeInput{
	FunctionName: aws.String("api"),
	Payload:      []byte(`{"id":1}`),
})
```
{% endtab %}
{% endtabs %}

## Calling one function from another

A function can invoke another through the same Lambda API. Set the endpoint in the calling function's `environment`, with the address the function reaches the dev server on:

```yaml
# .lambda.yml
environment:
  AWS_ENDPOINT_URL_LAMBDA: http://lambdary:8000
```

In [Docker Compose](docker-compose.md), functions share the Lambdary service's network and use `http://lambdary:8000`. On the process backend, functions run on your host and use `http://127.0.0.1:8000`.

## Protocols

- **Lambda:** the REST JSON API every SDK uses.
- **SQS:** the AWS JSON 1.0 protocol current SDKs use. SQS's older query protocol (`Action=SendMessage` form posts, sent by older SDKs) is not supported.
- **CloudWatch:** `GetMetricData` over Smithy RPCv2 CBOR, which current SDKs use.

Unsupported Lambda calls answer `501` `UnsupportedOperation`, so an SDK reports what Lambdary lacks rather than a generic not-found.

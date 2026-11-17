---
description: Publish function versions, create aliases and run weighted canaries locally.
---

# Versions and Aliases

`lambdary dev` serves Lambda's version and alias API, so the AWS CLI and SDKs work unchanged with an endpoint override:

```sh
export AWS_ENDPOINT_URL=http://127.0.0.1:8000

aws lambda publish-version --function-name notify --description "sha=abc123"
aws lambda create-alias --function-name notify --name live --function-version 1

# Send 10% of live's traffic to version 2
aws lambda update-alias --function-name notify --name live \
  --routing-config '{"AdditionalVersionWeights":{"2":0.1}}'

aws lambda invoke --function-name notify --qualifier live out.json
```

## Supported calls

`PublishVersion`, `ListVersionsByFunction`, `CreateAlias`, `GetAlias`, `UpdateAlias`, `GetFunctionConfiguration`, `GetFunction` (its `Code` location is empty), `ListFunctions`, and `Invoke` with a version number or alias as its qualifier. Any other Lambda control-plane call answers `501` `UnsupportedOperation`. See [AWS API](../reference/aws-api.md) for the details of each.

Each invoke response names the version that ran in `X-Amz-Executed-Version`.

## Versions

A version is an immutable copy of the function under `.lambdary/versions/`, so it survives restarts and keeps running the same code however you edit the source. Only `$LATEST`, the unqualified function, hot reloads.

As on AWS, publishing with nothing changed returns the latest version instead of a new one. `PublishVersion` also honors `CodeSha256` and `RevisionId` preconditions.

A snapshot includes everything `.lambda.yml` resolved, including values from `local.env_file` in plain text, so keep `.lambdary/versions/` out of version control.

A published version runs in its own container or process, next to `$LATEST`.

## Aliases

- Alias names are 1 to 128 letters, digits, `-` or `_`, and not all digits.
- An alias points at a published version or at `$LATEST`.
- `RoutingConfig` sends a share of the alias's traffic to one additional published version, with a weight between 0 and 1. An alias on `$LATEST` can't have one.
- `UpdateAlias` keeps any field you leave out, and honors a `RevisionId` precondition.

## Qualifiers

An invoke can name the version or alias as the function name's suffix (`notify:live`, or a full ARN ending in `:live`) or as the `Qualifier` query parameter. A qualifier in both places must match.

Function routes (`url.path`) always run `$LATEST`. SQS events and schedules accept `qualifier:` to invoke a version or alias instead; see [Queues and Schedules](queues-and-schedules.md).

## Logs and metrics per version

`lambdary logs notify` includes the output of every published version. `notify:2` selects one version, and `notify:live` the versions the alias routes to now. Metrics are split per version and alias too; see [Logs, Metrics and Health](logs-metrics-and-health.md).

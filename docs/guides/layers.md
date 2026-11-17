---
description: Use Lambda layers from layer version ARNs or local directories.
---

# Layers

`layers:` mirrors AWS Lambda's own Layers feature, not a local-only hint, so it lives at the top level of `.lambda.yml` like `runtime` or `environment`:

```yaml
runtime: provided.al2023
layers:                   # up to 5, merged into /opt in order (later entries win)
  - arn:aws:lambda:eu-west-1:534081306603:layer:php-83:1  # a layer version ARN (e.g. Bref's PHP layer)
  - ../shared-layer        # a local directory or .zip, relative to the function dir
```

Up to 5 entries, each a layer version ARN or a local path, are merged in declared order into `/opt`. Later entries win on file conflicts, exactly like AWS. `defaults.layers` in `lambdary.yml` applies to functions that set no `layers` of their own.

## Layer version ARNs

An entry starting with `arn:` must be a full layer **version** ARN:

```
arn:aws:lambda:<region>:<account-id>:layer:<name>:<version>
```

A versionless layer ARN is rejected. Partitions other than `aws`, such as `aws-cn` or `aws-us-gov`, are accepted.

ARN entries are fetched using your own AWS credentials, through the standard SDK credential chain. That is enough to pull public third-party layers too, such as Bref's PHP layers. The ARN's own region is used for the call. A layer private to another account fails with an access-denied error that says so.

Fetched content is cached under `.lambdary/layers/` and digest-pinned in `.lambdary/lock`, so a function keeps starting from the same layer version across machines. Commit `.lambdary/lock` alongside your functions.

## Local layers

Any other entry is a local path: a directory or a `.zip`, resolved relative to the function directory. Local layers need no AWS credentials at all. The path is checked when the function starts.

## Per backend

### Container backend

The merged layers are mounted read-only at `/opt`, as on AWS.

A Bref-style `provided.*` function can get its `bootstrap` straight from a layer, so no local `bootstrap` file is required. A `bootstrap` in the function directory still wins, matching AWS's own bootstrap search order.

### Process backend

`layers:` on the process backend works for pure-code layers. There is no real `/opt`, so the layers are made visible through the runtimes' own standard search paths:

| Runtime | Variables |
|---|---|
| `nodejs*` | `NODE_PATH` gains `nodejs/node_modules` and `nodejs/node<major>/node_modules` |
| `python*` | `PYTHONPATH` gains `python` and `python/lib/python<version>/site-packages` |
| `ruby*` | `RUBYLIB` gains `ruby/lib`, `GEM_PATH` gains `ruby/gems/<version>.0` |
| every runtime | `PATH` gains `bin` |

Layer content compiled for Amazon Linux (native binaries, Bref's `php`) needs the container backend: the same fidelity trade-off as the process backend in general.

A `provided.*` function whose `bootstrap` comes from a layer, with no local `bootstrap`, can't run as a host process at all. It falls back to the container backend automatically, with a notice.

## Published versions

[Publishing a version](versions-and-aliases.md) copies the function's resolved layers into the version, so the version keeps running the same layer content.

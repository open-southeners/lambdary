---
description: Run lambdary dev as a Docker Compose service next to the rest of your stack.
---

# Docker Compose

The `ghcr.io/open-southeners/lambdary` image runs `lambdary dev` as a Compose service, starting function containers next to it through the Docker socket:

```yaml
services:
  lambdary:
    image: ghcr.io/open-southeners/lambdary:latest
    command: ["--root", "functions"]        # added to `lambdary dev`
    user: "1000:1000"                       # your host user, so files stay yours
    group_add: ["988"]                      # the docker socket's group
    stop_grace_period: 40s                  # time to stop function containers
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - .:/workspace
      - lambdary-cache:/cache               # Go's build and module caches
volumes:
  lambdary-cache:
```

The image's entrypoint is `lambdary dev --host 0.0.0.0`, so `command:` adds flags to it. It listens on port `8000` and works in `/workspace`.

`stat -c %g /var/run/docker.sock` prints the group to use in `group_add`.

## Reaching functions

Other services reach it by name, such as `http://lambdary:8000/my-function` or an AWS SDK with `http://lambdary:8000` as its endpoint. Functions join the same network, so they reach those services by name too, and call each other through that same endpoint.

```yaml
services:
  app:
    environment:
      AWS_ENDPOINT_URL_LAMBDA: http://lambdary:8000
      AWS_ENDPOINT_URL_SQS: http://lambdary:8000
```

## Settings

It works out the rest from its own container: which host paths its mounts come from, and which network it is on. Two settings override that, and you need them if it can't inspect itself, such as when you set `hostname:` on the service:

| Setting | Meaning |
|---|---|
| `LAMBDARY_NETWORK` | Docker network for function containers. Required when the service is on several networks. Also works outside a container. |
| `LAMBDARY_HOST_ROOT` | Host path of the `--root` directory. |
| `LAMBDARY_ROOT` | Default for `--root`, an explicit `--root` still wins. |

The `--root` directory must be inside one of the service's mounts, so the host's Docker can mount it into function containers.

## Running commands in the service

`docker exec` runs in `/workspace`, not in the server's `--root`, so `docker exec lambdary lambdary list` would scan the wrong folder. Set `LAMBDARY_ROOT` in the service's `environment:` instead of passing `--root`, and the server and every command you exec use the same root:

```yaml
services:
  lambdary:
    image: ghcr.io/open-southeners/lambdary:latest
    environment:
      LAMBDARY_ROOT: functions
```

```sh
docker compose exec lambdary lambdary list
docker compose exec lambdary lambdary logs -f
```

## Healthcheck

`GET /_lambdary/health` answers `200` once the server is up. The image has busybox's `wget`:

```yaml
    healthcheck:
      test: ["CMD", "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8000/_lambdary/health"]
      interval: 5s
```

See [Logs, Metrics and Health](logs-metrics-and-health.md#health).

## What the image includes

The image includes Go and downloads the Go version your project asks for, so [Go functions](go-functions.md) build inside it. Its caches live in `/cache`; mount a volume there to keep them across restarts.

Only the container backend runs from it, because it has no Node, Python or Ruby.

## Stopping

Function containers are siblings of the Lambdary container, so Docker doesn't stop them with it. Lambdary stops them on shutdown; `stop_grace_period` gives it the time to. A hard kill, including Compose's after a too-short `stop_grace_period`, leaves them running. See [Limitations](../reference/limitations.md#containers-left-behind).

{% hint style="warning" %}
The Docker socket gives this service full control of the host's Docker, so use it for local development only. Outside a container, `lambdary dev --host 0.0.0.0` listens beyond loopback, with no authentication.
{% endhint %}

# The official Lambdary image: `lambdary dev` for a Docker Compose stack.
# See the README's "Running in Docker Compose" section.

FROM golang:1.26-alpine AS build

RUN apk add --no-cache bash git

WORKDIR /src
COPY . .

ARG VERSION=dev
RUN scripts/build.sh "${VERSION}"

# The runtime stage keeps the Go toolchain, because Go functions are built where
# Lambdary runs. It deliberately has no git: `go build` then skips VCS stamping,
# rather than failing with "dubious ownership" on a repository mounted from a
# different user.
FROM golang:1.26-alpine

RUN apk add --no-cache docker-cli docker-cli-buildx \
    && mkdir -m 1777 /cache /workspace

COPY --from=build /src/dist/lambdary /usr/local/bin/lambdary

# Writable under any `user:`, so Lambdary can run as the host user and files it
# writes to the source mount stay theirs. The golang image pins GOTOOLCHAIN to
# local; a project's own go.mod or go.work may need a newer Go than this one.
ENV HOME=/cache \
    GOCACHE=/cache/go-build \
    GOMODCACHE=/cache/mod \
    GOTOOLCHAIN=auto

WORKDIR /workspace

EXPOSE 8000

ENTRYPOINT ["lambdary", "dev", "--host", "0.0.0.0"]

#!/usr/bin/env bash
#
# Builds the lambdary binary for the current GOOS/GOARCH, embedding a copy of
# AWS's Lambda Runtime Interface Emulator the same way the release workflow
# does for its cross-compiled targets (.github/workflows/release.yml's
# `binaries` job) — just for one platform instead of all four, and without
# packaging a tarball. Output: dist/lambdary.
#
# Usage: scripts/build.sh [version]
#   version defaults to a `git describe` of the working tree.
#
# Skip the RIE embed (plain `go build ./cmd/lambdary` equivalent, falls back
# to building the emulator from source on first run) with:
#   LAMBDARY_SKIP_RIE_EMBED=1 scripts/build.sh
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
REPO_ROOT="$(pwd)"

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
GOOS="$(go env GOOS)"
GOARCH="$(go env GOARCH)"

mkdir -p dist

LDFLAGS="-s -w -X github.com/open-southeners/lambdary/internal/cli.version=${VERSION}"

if [ "${LAMBDARY_SKIP_RIE_EMBED:-}" = "1" ]; then
  echo "building lambdary ${VERSION} for ${GOOS}/${GOARCH} (no embedded RIE)..."
  go build -trimpath -ldflags "${LDFLAGS}" -o dist/lambdary ./cmd/lambdary
  echo "done: dist/lambdary"
  exit 0
fi

# internal/rie/rie.go's Version const is the single source of truth for the
# upstream RIE tag embedded below — see the WARNING comment next to that
# const for why this grep must keep matching its exact declaration form.
RIE_VERSION="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' internal/rie/rie.go)"
if [ -z "${RIE_VERSION}" ]; then
  echo "error: could not extract rie.Version from internal/rie/rie.go — grep pattern out of sync?" >&2
  exit 1
fi

embedded_path="internal/rie/embedded/aws-lambda-rie"

if [ ! -s "${embedded_path}" ]; then
  echo "building AWS Lambda runtime emulator ${RIE_VERSION} for ${GOOS}/${GOARCH}..."

  rie_src="$(mktemp -d)"
  trap 'rm -rf "${rie_src}"' EXIT

  git clone --depth 1 --branch "${RIE_VERSION}" \
    https://github.com/aws/aws-lambda-runtime-interface-emulator "${rie_src}"

  (cd "${rie_src}" && CGO_ENABLED=0 GOOS="${GOOS}" GOARCH="${GOARCH}" GOTOOLCHAIN=auto \
    go build -o "${REPO_ROOT}/${embedded_path}" ./cmd/aws-lambda-rie)

  if [ ! -s "${embedded_path}" ]; then
    echo "error: ${embedded_path} missing or empty after building RIE" >&2
    exit 1
  fi
else
  echo "reusing existing ${embedded_path} (delete it to rebuild for a different platform)"
fi

echo "building lambdary ${VERSION} for ${GOOS}/${GOARCH} (with embedded RIE)..."
CGO_ENABLED=0 go build -trimpath -tags embedrie -ldflags "${LDFLAGS}" -o dist/lambdary ./cmd/lambdary

echo "done: dist/lambdary"

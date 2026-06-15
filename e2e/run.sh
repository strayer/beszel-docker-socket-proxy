#!/bin/sh
# Run the e2e suite inside a container that shares a socket volume with the
# proxy under test. Use this on hosts where the test process can't dial a
# bind-mounted unix socket directly (macOS/OrbStack); on Linux you can also
# just run `go test ./e2e -tags e2e`.
#
#   PROXY_IMAGE=ghcr.io/strayer/beszel-docker-socket-proxy:dev ./e2e/run.sh -v
set -eu

: "${PROXY_IMAGE:?set PROXY_IMAGE to the proxy image under test}"

# Keep the toolchain image in sync with go.mod / the Dockerfile builder.
GO_IMAGE="${GO_IMAGE:-golang:1.26.4}"
DOCKER_SOCK="${DOCKER_SOCK:-/var/run/docker.sock}"
root="$(cd "$(dirname "$0")/.." && pwd)"
vol="bsp-e2e-sock-$$"

docker volume create "$vol" >/dev/null
trap 'docker volume rm -f "$vol" >/dev/null 2>&1 || true' EXIT

# The daemon socket is bound from $DOCKER_SOCK on the host to the canonical
# path inside the container; the test (and the proxy containers it starts)
# always use /var/run/docker.sock, so DOCKER_SOCK is not forwarded inward.
docker run --rm \
  -v "$DOCKER_SOCK":/var/run/docker.sock \
  -v "$vol":/run/beszel \
  -v "$root":/src -w /src \
  -e PROXY_IMAGE="$PROXY_IMAGE" \
  -e E2E_SOCK_VOLUME="$vol" \
  -e GOFLAGS=-buildvcs=false \
  "$GO_IMAGE" \
  go test ./e2e -tags e2e "$@"

# beszel-docker-socket-proxy

A minimal, filtering Docker socket proxy for the [Beszel](https://beszel.dev)
monitoring agent.

[![CI](https://github.com/strayer/beszel-docker-socket-proxy/actions/workflows/ci.yaml/badge.svg)](https://github.com/strayer/beszel-docker-socket-proxy/actions/workflows/ci.yaml)

```
ghcr.io/strayer/beszel-docker-socket-proxy
```

## Why

Generic socket proxies like [Tecnativa's docker-socket-proxy](https://github.com/Tecnativa/docker-socket-proxy)
(HAProxy) can only allow or deny request **paths** — they cannot touch
response **bodies**. Beszel's agent calls `GET /containers/{id}/json`
(container inspect), and that response carries `Config.Env`: every
environment variable of every container, API keys included, crosses the
socket into the Beszel process.

(Current Beszel deletes `Config.Env` agent-side before forwarding to the hub
— but the secrets still reach the agent process. Stripping them at the proxy
is the defense-in-depth fix: a compromised or regressed Beszel never sees
them.)

This proxy:

- allows **exactly** the six GET endpoints Beszel's agent uses (verified
  against the Beszel source) — everything else, including all non-GET
  methods, is denied with 403
- **strips `Config.Env`** from container inspect responses
- fails **closed**: if the inspect body cannot be parsed and re-encoded
  (or exceeds 8 MiB), the proxy answers 502 and the original body is never
  forwarded

## Allowed API surface

Verified against Beszel's `agent/docker.go` (May 2026). Beszel uses plain
unversioned paths (no `/v1.xx/` prefix), GET only — it does **not** use
`/events` or `/_ping`:

| Endpoint | Used for | Filtered |
|---|---|---|
| `GET /containers/json` | container list | – |
| `GET /containers/{id}/stats` | one-shot CPU/mem/net stats | – |
| `GET /containers/{id}/json` | Podman health workaround, hub "container info" view | **`Config.Env` removed** |
| `GET /containers/{id}/logs` | hub log viewer | – |
| `GET /version` | engine detection (incl. `Server` response header) | – |
| `GET /info` | host info | – |

Anything else → `403`.

## Design

Pure Go, stdlib only: `httputil.ReverseProxy` over the unix socket, Go
`ServeMux` allowlist routing, `Config.Env` stripped in `ModifyResponse`.
On filter failure the proxy returns **502** and never forwards the
unfiltered body. The final image is a single static binary `FROM scratch`.

Before settling on this implementation, nginx+Lua (OpenResty and Alpine)
and nginx+njs body-filter prototypes were built and benchmarked. At
Beszel's real request rate (~2 req/s) every variant cost ≈0.1 % of one
core and a few MB of RAM, so runtime footprint did not differentiate them.
The decision rested on **supply chain security and code complexity**, where
the Go proxy wins outright: zero third-party runtime artifacts (scratch +
one self-built binary, no base-image CVE treadmill), a single trust anchor,
a few hundred lines in one language, and a provable fail-closed 502 — the
nginx variants cannot change the response status once the body filter runs,
so their failure mode is a 200 with a stub body.

## Configuration

| Env var | Default | |
|---|---|---|
| `SOCKET_PATH` | `/var/run/docker.sock` | Docker socket to proxy |
| `LISTEN_ADDR` | `:2375` | listen address |

## Deployment (drop-in for Tecnativa)

```yaml
services:
  docker-socket-proxy:
    image: ghcr.io/strayer/beszel-docker-socket-proxy:1
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    cap_drop: [ALL]
    read_only: true
    security_opt: ["no-new-privileges:true"]
    networks: [beszel-internal]

  beszel-agent:
    image: henrygd/beszel-agent
    environment:
      DOCKER_HOST: tcp://docker-socket-proxy:2375
      # ...
    networks: [beszel-internal]

networks:
  beszel-internal:
    internal: true
```

Notes:

- The container runs as root *inside* the container so it can read the
  socket regardless of the host's `docker` GID, but with `cap_drop: ALL`,
  `no-new-privileges` and a read-only rootfs.
- The `:ro` socket mount protects the socket *file*, not the API — the
  proxy's GET-only allowlist is the actual write protection.
- Don't publish the proxy port to the host; keep it on an internal network.

## Development

Tooling is pinned in `mise.toml` (`mise install`); hooks in
`.pre-commit-config.yaml`.

```sh
go test ./... -cover        # unit tests (filter, fail-closed, routing table)
go vet ./... && golangci-lint run --build-tags e2e
```

### E2E suite

`e2e/` verifies the **final compiled image** against a real Docker daemon,
with the direct socket as oracle. It covers every exposed endpoint, a
default-deny matrix (mutations, method abuse, path traversal — proven
side-effect-free), and the fail-closed guarantees via a mock daemon that
serves broken inspect responses (invalid JSON, >8 MiB, hangs):

```sh
docker build -t beszel-socket-proxy:dev .
PROXY_IMAGE=beszel-socket-proxy:dev go test ./e2e -tags e2e -v
```

The suite creates nonce-named throwaway containers (`bsp-e2e-*`) and
removes them afterwards. CI runs it on every PR against the candidate
image, and again as the gate before any release is published.

## Releases

Pushing a tag `v*` runs the full unit + e2e gate against the freshly built
image and then publishes multi-arch (`linux/amd64`, `linux/arm64`) images
with SBOM and provenance to ghcr, tagged `{version}`, `{major}.{minor}`,
`{major}` and `latest`. Every push to `main` publishes a `dev` tag.

Because the image has zero third-party dependencies, its entire
vulnerability surface is the Go standard library and toolchain. That is
exactly what `govulncheck` tracks: it runs in source mode on every PR, and
a weekly scheduled job scans the binary inside the *latest published image*
so a disclosure between releases still surfaces. Renovate keeps the Go
toolchain (and everything else) current — the fix for a finding is merging
the already-open Renovate PR and tagging.

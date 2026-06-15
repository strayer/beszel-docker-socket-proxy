# beszel-docker-socket-proxy

A minimal, filtering Docker socket proxy for the [Beszel](https://beszel.dev)
monitoring agent.

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

Pure Go, stdlib only: `httputil.ReverseProxy` over the upstream Docker
socket, Go `ServeMux` allowlist routing, `Config.Env` stripped in
`ModifyResponse`. On filter failure the proxy returns **502** and never
forwards the unfiltered body. The final image is a single static binary
`FROM scratch`.

The proxy itself listens on a **unix domain socket** (no TCP port). The
Beszel agent must run with `network_mode: host`, so it can't join a Docker
network — a shared volume holding the socket is how it reaches the proxy
without publishing a port on the host.

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
| `SOCKET_PATH` | `/var/run/docker.sock` | upstream Docker socket to proxy |
| `LISTEN_ADDR` | `/run/beszel/docker.sock` | path of the unix socket the proxy creates and serves on (a leading `unix:` is tolerated) |
| `LISTEN_SOCKET_MODE` | `0600` | permission bits of the created socket (octal) |

## Deployment

The proxy serves on a unix socket inside a volume shared with the agent:

```yaml
services:
  docker-socket-proxy:
    image: ghcr.io/strayer/beszel-docker-socket-proxy:1
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
      - beszel-socket:/run/beszel
    cap_drop: [ALL]
    read_only: true
    security_opt: ["no-new-privileges:true"]

  beszel-agent:
    image: henrygd/beszel-agent
    restart: unless-stopped
    network_mode: host
    environment:
      DOCKER_HOST: unix:///run/beszel/docker.sock
      # ...
    volumes:
      - beszel-socket:/run/beszel

volumes:
  beszel-socket:
```

Notes:

- The proxy creates its socket in the `beszel-socket` volume
  (`LISTEN_ADDR`, default `/run/beszel/docker.sock`). Because the agent runs
  `network_mode: host`, the shared volume — not a Docker network — is the
  channel between them.
- Both containers run as root, so the default socket mode `0600`
  (root-owned) lets the agent use it while keeping it inaccessible to
  anything else. If your agent runs as a non-root user, set
  `LISTEN_SOCKET_MODE` to a group-writable mode (e.g. `0660` — connecting to
  a unix socket needs write permission on it) and give both containers a
  shared `group_add` instead.
- The agent mounts the volume **read-write**: a client has to write to the
  socket to send requests. The write protection against the Docker API is
  the proxy's GET-only allowlist, not the mount.
- The host socket is mounted `:ro`; the proxy runs with `cap_drop: ALL`,
  `no-new-privileges` and a read-only rootfs.

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

The test dials the proxy's unix socket directly, which works on a Linux
host (and CI). On macOS a bind-mounted unix socket isn't dialable from the
host, so run the suite through the wrapper, which executes it in a
container sharing a socket volume with the proxy:

```sh
PROXY_IMAGE=beszel-socket-proxy:dev ./e2e/run.sh -v
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

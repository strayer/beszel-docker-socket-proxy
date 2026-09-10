FROM --platform=$BUILDPLATFORM golang:1.27.1@sha256:f44f6e88636cfb311f9ebace870ded69d943f227bb3cb27d32ffd84ea18c43ea AS build

ARG TARGETOS TARGETARCH

WORKDIR /src

COPY go.mod ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /proxy ./cmd/proxy/

FROM scratch

COPY --from=build /proxy /proxy

# Runs as root inside the container so it can read the Docker socket
# regardless of the host's docker GID; deploy with cap_drop: ALL,
# read_only and no-new-privileges (see README). The proxy serves on a unix
# socket inside a mounted volume (LISTEN_ADDR), not a TCP port.

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD ["/proxy", "healthcheck"]

ENTRYPOINT ["/proxy"]

FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build

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

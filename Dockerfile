FROM --platform=$BUILDPLATFORM golang:1.26.4@sha256:32c0e6e5c4f6707717051091b4d0b077464a679eaab563e11474efc5328e2aa5 AS build

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

ENTRYPOINT ["/proxy"]

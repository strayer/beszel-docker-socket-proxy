// beszel-docker-socket-proxy is a minimal filtering Docker socket proxy for
// the Beszel monitoring agent. See internal/proxy for the actual behavior.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/strayer/beszel-docker-socket-proxy/internal/proxy"
)

const (
	defaultSocketPath = "/var/run/docker.sock"
	// defaultListenAddr is a path inside the shared volume the operator
	// mounts; the Beszel agent reaches it via DOCKER_HOST=unix://<path>.
	defaultListenAddr = "/run/beszel/docker.sock"
	// defaultListenMode keeps the socket usable only by root (owner), which
	// both this proxy and the agent run as. Override with LISTEN_SOCKET_MODE.
	defaultListenMode = "0600"
)

func main() {
	socketPath := envOr("SOCKET_PATH", defaultSocketPath)
	listenAddr := envOr("LISTEN_ADDR", defaultListenAddr)

	mode, err := strconv.ParseUint(envOr("LISTEN_SOCKET_MODE", defaultListenMode), 8, 32)
	if err != nil {
		log.Fatalf("invalid LISTEN_SOCKET_MODE: %v", err)
	}

	ln, err := proxy.Listen(listenAddr, os.FileMode(mode))
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Handler:           proxy.NewHandler(socketPath),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout intentionally 0: log responses may stream.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("listening on %s, proxying %s", ln.Addr(), socketPath)
	if err := proxy.Serve(ctx, server, ln, 10*time.Second); err != nil {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// beszel-docker-socket-proxy is a minimal filtering Docker socket proxy for
// the Beszel monitoring agent. See internal/proxy for the actual behavior.
package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/strayer/beszel-docker-socket-proxy/internal/proxy"
)

const (
	defaultSocketPath = "/var/run/docker.sock"
	defaultListenAddr = ":2375"
)

func main() {
	socketPath := envOr("SOCKET_PATH", defaultSocketPath)
	listenAddr := envOr("LISTEN_ADDR", defaultListenAddr)

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           proxy.NewHandler(socketPath),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       15 * time.Second,
		// WriteTimeout intentionally 0: log responses may stream.
		IdleTimeout:    60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}

	log.Printf("listening on %s, proxying %s", listenAddr, socketPath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

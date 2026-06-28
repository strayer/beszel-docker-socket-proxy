// beszel-docker-socket-proxy is a minimal filtering Docker socket proxy for
// the Beszel monitoring agent. See internal/proxy for the actual behavior.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
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
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := envOr("LISTEN_ADDR", defaultListenAddr)
		if err := healthcheck(proxy.SocketPath(addr)); err != nil {
			log.Printf("healthcheck: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

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

// healthcheck dials the proxy's own listen socket and performs GET /version,
// returning nil only on a 200 response. It is the container HEALTHCHECK: the
// scratch image has no shell or curl, so the binary checks itself.
func healthcheck(sock string) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
	resp, err := client.Get("http://proxy/version")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

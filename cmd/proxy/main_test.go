package main

import (
	"net"
	"net/http"
	"path/filepath"
	"testing"
)

// startHealthServer serves handler on a unix socket in a temp dir and returns
// the socket path, standing in for the running proxy.
func startHealthServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// TestHealthcheckHealthy: a server answering 200 on GET /version is healthy.
func TestHealthcheckHealthy(t *testing.T) {
	sock := startHealthServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/version" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"test"}`))
	}))
	if err := healthcheck(sock); err != nil {
		t.Fatalf("healthcheck on healthy server: %v", err)
	}
}

// TestHealthcheckUnhealthy: a non-200 response is unhealthy.
func TestHealthcheckUnhealthy(t *testing.T) {
	sock := startHealthServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if err := healthcheck(sock); err == nil {
		t.Fatal("healthcheck on 500 server returned nil, want error")
	}
}

// TestHealthcheckNotListening: no server on the socket path is unhealthy.
func TestHealthcheckNotListening(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "absent.sock")
	if err := healthcheck(sock); err == nil {
		t.Fatal("healthcheck with no listener returned nil, want dial error")
	}
}

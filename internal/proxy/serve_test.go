package proxy

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestServeGracefulShutdown: Serve runs until its context is cancelled, then
// shuts the server down cleanly (returning nil) and unlinks the listen
// socket so a restart finds a clean path.
func TestServeGracefulShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := Listen(path, 0o600)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}

	srv := &http.Server{Handler: NewHandler(markerUpstream(t))}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- Serve(ctx, srv, ln, 5*time.Second) }()

	// Wait until the proxy answers, proving Serve is up.
	client := unixClient(path)
	if err := waitReady(client); err != nil {
		t.Fatalf("proxy never came up: %v", err)
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error on graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}

	// A clean shutdown unlinks the unix socket.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("listen socket not removed after shutdown: stat err = %v", err)
	}
}

func unixClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
}

func waitReady(client *http.Client) error {
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		var resp *http.Response
		if resp, err = client.Get("http://proxy/version"); err == nil {
			_ = resp.Body.Close()
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return err
}

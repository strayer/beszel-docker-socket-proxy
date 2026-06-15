package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSocketPath(t *testing.T) {
	for addr, want := range map[string]string{
		"/run/beszel/docker.sock":        "/run/beszel/docker.sock",
		"unix:/run/beszel/docker.sock":   "/run/beszel/docker.sock",
		"unix:///run/beszel/docker.sock": "/run/beszel/docker.sock",
		"unix:/a/b":                      "/a/b",
	} {
		if got := socketPath(addr); got != want {
			t.Errorf("socketPath(%q) = %q, want %q", addr, got, want)
		}
	}
}

// TestListenServesAndChmods listens on a unix socket, serves the full
// handler against a fake upstream, and checks the socket mode is applied.
func TestListenServesAndChmods(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")
	ln, err := Listen("unix:"+path, 0o600)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("created path is not a socket: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %o, want 600", perm)
	}

	srv := &http.Server{Handler: NewHandler(markerUpstream(t))}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}
	resp, err := client.Get("http://proxy/version")
	if err != nil {
		t.Fatalf("GET over unix socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"upstream"`) {
		t.Fatalf("GET /version: status %d, body %s", resp.StatusCode, body)
	}
}

// TestListenRemovesStaleSocket: a socket file left by an unclean shutdown
// must not block a fresh listen.
func TestListenRemovesStaleSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")

	// Leave a stale socket behind: SetUnlinkOnClose(false) keeps the file
	// after Close, simulating a crash.
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed stale socket: %v", err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale socket not present: %v", err)
	}

	ln, err := Listen(path, 0o600)
	if err != nil {
		t.Fatalf("Listen over stale socket: %v", err)
	}
	_ = ln.Close()
}

// TestListenRefusesLiveSocket: a socket with an active listener must not be
// removed and taken over.
func TestListenRefusesLiveSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.sock")
	live, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("seed live socket: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	if _, err := Listen(path, 0o600); err == nil {
		t.Fatal("Listen succeeded over a live socket, want error")
	}
	// The original listener must still be accepting.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live socket was removed: %v", err)
	}
	conn, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatalf("original listener no longer accepting: %v", err)
	}
	_ = conn.Close()
}

// TestListenRefusesNonSocket: Listen must never delete a regular file.
func TestListenRefusesNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "important.txt")
	if err := os.WriteFile(path, []byte("do not delete me"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Listen(path, 0o600); err == nil {
		t.Fatal("Listen succeeded on a regular file, want error")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("regular file was removed: %v", err)
	}
	if string(data) != "do not delete me" {
		t.Fatalf("regular file was modified: %q", data)
	}
}

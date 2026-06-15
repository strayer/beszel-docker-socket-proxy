//go:build e2e

package e2e

import (
	"os"
	"testing"
)

// TestListenSocketMode confirms the running image created its listen socket
// with the mode requested via LISTEN_SOCKET_MODE (the harness sets 0666).
// The default 0600 is covered by the proxy package's unit tests.
func TestListenSocketMode(t *testing.T) {
	fi, err := os.Stat(dialPath(proxySockName))
	if err != nil {
		t.Fatalf("stat listen socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("listen path is not a unix socket: %v", fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Errorf("socket mode = %o, want 666 (LISTEN_SOCKET_MODE honored)", perm)
	}
}

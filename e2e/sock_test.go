//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	// proxySockDir is where the proxy and mock containers mount the shared
	// socket directory; proxySockName is the main proxy's listen socket
	// (matches the default LISTEN_ADDR basename).
	proxySockDir  = "/run/beszel"
	proxySockName = "docker.sock"
)

var (
	sockMount   string // "<src>:/run/beszel" Binds entry for the containers
	sockDialDir string // directory the test process reads the sockets from
)

func dialPath(name string) string { return filepath.Join(sockDialDir, name) }

// resolveSock decides where the proxy's unix listen socket lives and how the
// test process reaches it, returning a cleanup func.
//
//   - Container mode (e2e/run.sh sets E2E_SOCK_VOLUME): the wrapper mounted
//     that named volume into the test container at proxySockDir, and we mount
//     the same volume into the proxy/mock containers. Works on any host.
//   - Native mode: a host temp dir bind-mounted into the containers and
//     dialed directly. Only works where the test process shares the daemon's
//     filesystem and can reach the socket — i.e. a Linux host.
func resolveSock() (func(), error) {
	if vol := os.Getenv("E2E_SOCK_VOLUME"); vol != "" {
		sockMount = vol + ":" + proxySockDir
		sockDialDir = proxySockDir
		return func() {}, nil
	}
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("e2e dials the proxy's unix socket directly, which only works on a Linux host; on %s run ./e2e/run.sh (runs the suite in a container)", runtime.GOOS)
	}
	dir, err := os.MkdirTemp("", "bsp-e2e-sock-*")
	if err != nil {
		return nil, err
	}
	// The proxy container (root) creates the socket here; make the dir
	// world-traversable so the non-root test user can reach the socket.
	if err := os.Chmod(dir, 0o777); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	sockMount = dir + ":" + proxySockDir
	sockDialDir = dir
	return func() { _ = os.RemoveAll(dir) }, nil
}

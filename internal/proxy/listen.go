package proxy

import (
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// umaskMu serializes the umask get/set around net.Listen. Listen is called
// once at startup in production; the lock just keeps the global umask
// mutation honest if it is ever called concurrently (e.g. in tests).
var umaskMu sync.Mutex

// Listen creates the unix domain socket the proxy serves on. addr is a
// filesystem path; a leading "unix:" (or "unix://") prefix is tolerated so
// the value can mirror a DOCKER_HOST string. The socket is created with the
// given file mode.
//
// The deployment assumes a single proxy owns the listen directory (a
// dedicated shared volume). Under that precondition:
//   - It refuses a path that exists and is not a socket, and never follows a
//     symlink onto one (Lstat does not dereference; bind will not overwrite
//     an existing path).
//   - It removes a socket left by an unclean shutdown only after confirming
//     nothing is accepting on it, so it won't displace a live listener. (It
//     does not guard against a second proxy racing for the same path
//     concurrently — that is out of scope for the single-writer deployment.)
//   - The socket is created with its final mode atomically via umask, so it
//     is never briefly world-accessible between bind and a chmod.
func Listen(addr string, mode os.FileMode) (net.Listener, error) {
	path := socketPath(addr)

	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("listen %s: path exists and is not a socket", path)
		}
		if c, derr := net.DialTimeout("unix", path, 200*time.Millisecond); derr == nil {
			_ = c.Close()
			return nil, fmt.Errorf("listen %s: socket already in use", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("listen %s: remove stale socket: %w", path, err)
		}
	}

	umaskMu.Lock()
	old := syscall.Umask(int(0o777 &^ mode.Perm()))
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		return nil, err
	}
	return ln, nil
}

// socketPath strips an optional unix:[//] scheme from addr.
func socketPath(addr string) string {
	if rest, ok := strings.CutPrefix(addr, "unix:"); ok {
		return "/" + strings.TrimLeft(rest, "/")
	}
	return addr
}

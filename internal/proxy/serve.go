package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Serve runs srv on ln until ctx is cancelled (e.g. on SIGTERM), then
// gracefully shuts the server down, waiting up to grace for in-flight
// requests to finish. A clean shutdown returns nil; closing ln also unlinks
// the unix socket so a restart finds a clean path.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, grace time.Duration) error {
	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serveErr <- err
	}()

	select {
	case err := <-serveErr:
		// Serve stopped on its own (listener died); no shutdown to do.
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), grace)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// Package proxy implements a minimal filtering Docker socket proxy for the
// Beszel monitoring agent.
//
// It exposes exactly the Docker Engine API endpoints Beszel's agent uses
// (GET-only) and strips Config.Env from container inspect responses so that
// secrets in environment variables never leave the Docker socket. Everything
// else is denied with 403.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strconv"
	"time"
)

// maxInspectBody bounds memory used when buffering an inspect response
// for filtering. Inspect payloads are typically 10-20 KB.
const maxInspectBody = 8 << 20 // 8 MiB

// containerIDPattern matches container IDs and names. Beszel passes either a
// 12/64-char hex ID or a container name; both fall within this charset.
var containerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// NewHandler builds the proxy's complete HTTP handler: the allowlist routing
// table over two reverse proxies (passthrough and Env-stripping inspect)
// dialing the Docker socket at socketPath.
func NewHandler(socketPath string) http.Handler {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socketPath)
		},
		MaxIdleConns:          8,
		IdleConnTimeout:       30 * time.Second,
		DisableCompression:    true,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	rewrite := func(pr *httputil.ProxyRequest) {
		pr.Out.URL.Scheme = "http"
		pr.Out.URL.Host = "docker"
		pr.Out.Host = "docker"
		// GET-only API surface: never forward a request body.
		pr.Out.Body = http.NoBody
		pr.Out.ContentLength = 0
		pr.Out.Header.Del("Content-Length")
	}

	errorHandler := func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %s %s: %v", r.Method, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"message":"proxy: upstream error"}`))
	}

	// passthrough proxies allowed endpoints unmodified. FlushInterval -1
	// flushes immediately so log streaming works.
	passthrough := &httputil.ReverseProxy{
		Transport:     transport,
		Rewrite:       rewrite,
		FlushInterval: -1,
		ErrorHandler:  errorHandler,
		ErrorLog:      log.Default(),
	}

	// inspect additionally strips Config.Env from the response body. Any
	// failure while filtering fails closed: ModifyResponse returns an error,
	// ErrorHandler emits 502, and the unfiltered body is never forwarded.
	inspect := &httputil.ReverseProxy{
		Transport:      transport,
		Rewrite:        rewrite,
		ErrorHandler:   errorHandler,
		ErrorLog:       log.Default(),
		ModifyResponse: stripEnvFromInspect,
	}

	mux := http.NewServeMux()
	mux.Handle("GET /containers/json", passthrough)
	mux.Handle("GET /containers/{id}/stats", validateID(passthrough))
	mux.Handle("GET /containers/{id}/json", validateID(inspect))
	mux.Handle("GET /containers/{id}/logs", validateID(passthrough))
	mux.Handle("GET /version", passthrough)
	mux.Handle("GET /info", passthrough)
	mux.HandleFunc("/", deny)

	return mux
}

// stripEnvFromInspect removes Config.Env from a container inspect response.
// Returning an error makes ReverseProxy invoke ErrorHandler (502) without
// forwarding any of the original body.
func stripEnvFromInspect(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		// Error responses (404 etc.) carry no inspect payload.
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInspectBody+1))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("inspect read: %w", err)
	}
	if len(body) > maxInspectBody {
		return errors.New("inspect response exceeds size limit")
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("inspect parse: %w", err)
	}
	if config, ok := doc["Config"].(map[string]any); ok {
		delete(config, "Env")
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("inspect marshal: %w", err)
	}

	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Transfer-Encoding")
	return nil
}

// validateID rejects requests whose {id} path segment contains characters
// outside the container ID/name charset.
func validateID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !containerIDPattern.MatchString(r.PathValue("id")) {
			deny(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func deny(w http.ResponseWriter, r *http.Request) {
	log.Printf("denied: %s %s", r.Method, r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"forbidden by beszel-docker-socket-proxy"}`))
}

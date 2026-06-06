//go:build e2e

// Package e2e verifies the final compiled proxy image against a real Docker
// daemon. The image under test comes from PROXY_IMAGE; the direct socket
// (DOCKER_SOCK, default /var/run/docker.sock) serves as the oracle.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// fixtureImage is the image used for the fixture containers.
// renovate: datasource=docker depName=alpine
const fixtureImage = "alpine:3.23.4"

const maxBody = 32 << 20

var (
	docker *engineClient // direct socket: the oracle and harness driver

	proxyHTTP *http.Client
	proxyBase string // http://127.0.0.1:<mapped port>

	nonce  string
	secret string // sentinel env value that must never cross the proxy

	envCtr fixture // tty=false: multiplexed log framing
	ttyCtr fixture // tty=true: raw log framing
)

type fixture struct {
	id   string
	name string
}

// engineClient is a minimal Docker Engine API client over the unix socket.
type engineClient struct{ http *http.Client }

func newEngineClient(sock string) *engineClient {
	return &engineClient{http: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", sock)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (e *engineClient) do(method, path string, body any) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://docker"+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return e.http.Do(req)
}

// call performs a request, asserts the status code, and optionally
// unmarshals the response body into out.
func (e *engineClient) call(method, path string, body, out any, wantStatus int) error {
	resp, err := e.do(method, path, body)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("%s %s: read: %w", method, path, err)
	}
	if resp.StatusCode != wantStatus {
		return fmt.Errorf("%s %s: status %d (want %d): %s", method, path, resp.StatusCode, wantStatus, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: parse: %w", method, path, err)
		}
	}
	return nil
}

func TestMain(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e harness: %v\n", err)
		code = 1
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	proxyImage := os.Getenv("PROXY_IMAGE")
	if proxyImage == "" {
		return 1, fmt.Errorf("PROXY_IMAGE must name the proxy image to test")
	}
	sock := os.Getenv("DOCKER_SOCK")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	docker = newEngineClient(sock)

	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return 1, err
	}
	nonce = hex.EncodeToString(buf)
	secret = "leakcheck-" + nonce

	if err := ensureImage(fixtureImage); err != nil {
		return 1, fmt.Errorf("fixture image: %w", err)
	}

	var started []string
	defer func() {
		for _, id := range started {
			_ = docker.call("DELETE", "/containers/"+id+"?force=true", nil, nil, 204)
		}
	}()

	// The proxy container runs exactly as deployed: read-only socket,
	// cap_drop ALL, read-only rootfs, no-new-privileges. Port 2375 is
	// published to an ephemeral localhost port for the test client.
	proxyID, err := startContainer("bsp-e2e-proxy-"+nonce, map[string]any{
		"Image":        proxyImage,
		"ExposedPorts": map[string]any{"2375/tcp": map[string]any{}},
		"HostConfig": map[string]any{
			"Binds":          []string{sock + ":/var/run/docker.sock:ro"},
			"CapDrop":        []string{"ALL"},
			"ReadonlyRootfs": true,
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"PortBindings":   map[string]any{"2375/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}},
		},
	})
	if err != nil {
		return 1, fmt.Errorf("proxy container: %w", err)
	}
	started = append(started, proxyID)

	port, err := mappedPort(proxyID, "2375/tcp")
	if err != nil {
		return 1, err
	}
	proxyBase = "http://127.0.0.1:" + port
	proxyHTTP = &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if err := waitFor("proxy ready", func() error {
		resp, err := proxyHTTP.Get(proxyBase + "/version")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}); err != nil {
		return 1, err
	}

	// Fixtures: known env sentinel, label, and deterministic log lines.
	fixtureCmd := []string{"sh", "-c",
		"echo e2e-stdout-one; echo e2e-stdout-two; echo e2e-stderr-one >&2; exec sleep 3600"}
	for _, f := range []struct {
		dst *fixture
		tty bool
	}{
		{&envCtr, false},
		{&ttyCtr, true},
	} {
		name := "bsp-e2e-env-" + nonce
		if f.tty {
			name = "bsp-e2e-tty-" + nonce
		}
		id, err := startContainer(name, map[string]any{
			"Image":  fixtureImage,
			"Cmd":    fixtureCmd,
			"Env":    []string{"E2E_SECRET=" + secret},
			"Labels": map[string]string{"beszel-docker-socket-proxy.e2e": nonce},
			"Tty":    f.tty,
		})
		if err != nil {
			return 1, fmt.Errorf("fixture %s: %w", name, err)
		}
		started = append(started, id)
		*f.dst = fixture{id: id, name: name}

		// Wait until the fixture's log lines are flushed so log
		// assertions are deterministic.
		if err := waitFor(name+" logs", func() error {
			data, _, err := directLogs(id, "stdout=1&stderr=1")
			if err != nil {
				return err
			}
			if !bytes.Contains(data, []byte("e2e-stderr-one")) {
				return fmt.Errorf("log lines not yet flushed")
			}
			return nil
		}); err != nil {
			return 1, err
		}
	}

	return m.Run(), nil
}

func ensureImage(img string) error {
	resp, err := docker.do("GET", "/images/"+img+"/json", nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}

	name, tag, _ := strings.Cut(img, ":")
	pull, err := docker.do("POST", "/images/create?fromImage="+name+"&tag="+tag, nil)
	if err != nil {
		return err
	}
	defer func() { _ = pull.Body.Close() }()
	if _, err := io.Copy(io.Discard, pull.Body); err != nil {
		return err
	}
	if pull.StatusCode != http.StatusOK {
		return fmt.Errorf("pull %s: status %d", img, pull.StatusCode)
	}
	return nil
}

func startContainer(name string, spec map[string]any) (string, error) {
	var created struct{ Id string }
	if err := docker.call("POST", "/containers/create?name="+name, spec, &created, 201); err != nil {
		return "", err
	}
	if err := docker.call("POST", "/containers/"+created.Id+"/start", nil, nil, 204); err != nil {
		// Don't leave the created-but-unstarted container behind; it
		// would never make it into the caller's cleanup list.
		_ = docker.call("DELETE", "/containers/"+created.Id+"?force=true", nil, nil, 204)
		return "", err
	}
	return created.Id, nil
}

func mappedPort(id, port string) (string, error) {
	var info struct {
		NetworkSettings struct {
			Ports map[string][]struct{ HostPort string }
		}
	}
	if err := docker.call("GET", "/containers/"+id+"/json", nil, &info, 200); err != nil {
		return "", err
	}
	bindings := info.NetworkSettings.Ports[port]
	if len(bindings) == 0 || bindings[0].HostPort == "" {
		return "", fmt.Errorf("no host binding for %s on %s", port, id)
	}
	return bindings[0].HostPort, nil
}

func waitFor(what string, probe func() error) error {
	deadline := time.Now().Add(15 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = probe(); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s: %w", what, err)
}

// proxyDo sends a request to the proxy under test and returns the response
// plus its full body.
func proxyDo(t *testing.T, method, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, proxyBase+path, nil)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	resp, err := proxyHTTP.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return resp, body
}

// directDo sends the same request over the direct socket (the oracle).
func directDo(t *testing.T, method, path string) (*http.Response, []byte) {
	t.Helper()
	resp, err := docker.do(method, path, nil)
	if err != nil {
		t.Fatalf("direct %s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		t.Fatalf("direct %s %s: read: %v", method, path, err)
	}
	return resp, body
}

func directLogs(id, params string) ([]byte, string, error) {
	resp, err := docker.do("GET", "/containers/"+id+"/logs?"+params, nil)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("logs %s: status %d", id, resp.StatusCode)
	}
	return data, resp.Header.Get("Content-Type"), nil
}

func parseJSON(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	return v
}

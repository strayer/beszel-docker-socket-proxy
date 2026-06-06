//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const mockMarker = "MOCK-SECRET-BYTES"

// setupMockProxy builds the mockdaemon image, runs it with its unix socket
// on a shared volume, and starts a second proxy instance from PROXY_IMAGE
// with SOCKET_PATH pointed at the mock. Returns the proxy's base URL.
func setupMockProxy(t *testing.T) string {
	t.Helper()

	image := "bsp-e2e-mockdaemon:" + nonce
	volume := "bsp-e2e-mock-" + nonce
	buildMockImage(t, image)
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/images/"+image+"?force=true", nil, nil, 200)
	})

	if err := docker.call("POST", "/volumes/create", map[string]any{"Name": volume}, nil, 201); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/volumes/"+volume, nil, nil, 204)
	})

	mockID, err := startContainer("bsp-e2e-mockdaemon-"+nonce, map[string]any{
		"Image": image,
		"Env":   []string{"MOCK_SOCKET=/shared/mock.sock"},
		"HostConfig": map[string]any{
			"Binds": []string{volume + ":/shared"},
		},
	})
	if err != nil {
		t.Fatalf("mockdaemon container: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/containers/"+mockID+"?force=true", nil, nil, 204)
	})

	proxyID, err := startContainer("bsp-e2e-proxymock-"+nonce, map[string]any{
		"Image":        os.Getenv("PROXY_IMAGE"),
		"Env":          []string{"SOCKET_PATH=/shared/mock.sock"},
		"ExposedPorts": map[string]any{"2375/tcp": map[string]any{}},
		"HostConfig": map[string]any{
			"Binds":          []string{volume + ":/shared"},
			"CapDrop":        []string{"ALL"},
			"ReadonlyRootfs": true,
			"SecurityOpt":    []string{"no-new-privileges:true"},
			"PortBindings":   map[string]any{"2375/tcp": []map[string]string{{"HostIp": "127.0.0.1", "HostPort": ""}}},
		},
	})
	if err != nil {
		t.Fatalf("proxy-on-mock container: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/containers/"+proxyID+"?force=true", nil, nil, 204)
	})

	port, err := mappedPort(proxyID, "2375/tcp")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://127.0.0.1:" + port
	if err := waitFor("proxy-on-mock ready", func() error {
		resp, err := proxyHTTP.Get(base + "/version")
		if err != nil {
			return err
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return base
}

// buildMockImage cross-compiles e2e/mockdaemon for the daemon's platform
// and packs the static binary into a FROM scratch image via the Engine
// API, so no Go toolchain image is ever pulled.
func buildMockImage(t *testing.T, tag string) {
	t.Helper()

	var ver struct{ Os, Arch string }
	if err := docker.call("GET", "/version", nil, &ver, 200); err != nil {
		t.Fatalf("daemon version: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "mockdaemon")
	cmd := exec.Command("go", "build", "-o", bin, "./mockdaemon")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+ver.Os, "GOARCH="+ver.Arch)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mockdaemon: %v\n%s", err, out)
	}
	binData, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}

	dockerfile := "FROM scratch\nCOPY mockdaemon /mockdaemon\nENTRYPOINT [\"/mockdaemon\"]\n"
	var ctx bytes.Buffer
	tw := tar.NewWriter(&ctx)
	for _, f := range []struct {
		name string
		mode int64
		data []byte
	}{
		{"Dockerfile", 0o644, []byte(dockerfile)},
		{"mockdaemon", 0o755, binData},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(f.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, "http://docker/build?t="+tag, &ctx)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-tar")
	resp, err := docker.http.Do(req)
	if err != nil {
		t.Fatalf("image build: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The build endpoint streams JSON messages; failures appear as
	// {"error": ...} lines rather than a non-200 status.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct{ Error string }
		if err := dec.Decode(&msg); err != nil {
			break
		}
		if msg.Error != "" {
			t.Fatalf("image build: %s", msg.Error)
		}
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("image build: status %d", resp.StatusCode)
	}
}

func mockGet(t *testing.T, base, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := proxyHTTP.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		t.Fatalf("GET %s: read: %v", path, err)
	}
	return resp, body
}

// TestFailClosed proves the property the Go implementation was chosen for,
// against the real image: when the inspect filter cannot run to completion,
// the client gets a 502 and zero bytes of the upstream body.
func TestFailClosed(t *testing.T) {
	base := setupMockProxy(t)

	assertFailClosed := func(t *testing.T, path string) {
		resp, body := mockGet(t, base, path)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("GET %s = %d, want 502", path, resp.StatusCode)
		}
		if bytes.Contains(body, []byte(mockMarker)) {
			t.Errorf("GET %s: upstream bytes leaked through failed filter", path)
		}
		if !bytes.Contains(body, []byte("proxy: upstream error")) {
			t.Errorf("GET %s: unexpected 502 body: %.200s", path, body)
		}
	}

	t.Run("invalid JSON", func(t *testing.T) {
		assertFailClosed(t, "/containers/badjson/json")
	})

	t.Run("oversize body", func(t *testing.T) {
		assertFailClosed(t, "/containers/huge/json")
	})

	t.Run("500 passes through untouched", func(t *testing.T) {
		resp, body := mockGet(t, base, "/containers/error500/json")
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", resp.StatusCode)
		}
		if want := `{"message":"mockdaemon: simulated daemon failure"}`; string(body) != want {
			t.Errorf("error body modified:\ngot  %s\nwant %s", body, want)
		}
	})

	// A hung upstream must fail within the proxy's response header
	// timeout (30s), and 50 abandoned client requests must not wedge
	// the proxy.
	t.Run("hung upstream", func(t *testing.T) {
		var wg sync.WaitGroup

		// One patient client observes the proxy-side timeout.
		var patientStatus int
		var patientErr error
		var patientTook time.Duration
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			resp, err := proxyHTTP.Get(base + "/containers/hang/json")
			patientTook = time.Since(start)
			if err != nil {
				patientErr = err
				return
			}
			_ = resp.Body.Close()
			patientStatus = resp.StatusCode
		}()

		// 50 impatient clients give up after 2s, leaving the proxy to
		// clean up the abandoned upstream requests.
		impatient := &http.Client{Timeout: 2 * time.Second}
		for range 50 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := impatient.Get(base + "/containers/hang/json")
				if err == nil {
					_ = resp.Body.Close()
				}
			}()
		}
		wg.Wait()

		if patientErr != nil {
			t.Fatalf("patient request failed client-side after %s: %v", patientTook, patientErr)
		}
		if patientStatus != http.StatusBadGateway {
			t.Errorf("hung upstream: status = %d, want 502", patientStatus)
		}
		if patientTook > 45*time.Second {
			t.Errorf("hung upstream: took %s, want < proxy timeout + slack", patientTook)
		}

		// The proxy must still work normally after the storm.
		if err := waitFor("proxy healthy after hang storm", func() error {
			resp, err := proxyHTTP.Get(base + "/version")
			if err != nil {
				return err
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("status %d", resp.StatusCode)
			}
			return nil
		}); err != nil {
			t.Error(err)
		}
		resp, body := mockGet(t, base, "/containers/badjson/json")
		if resp.StatusCode != http.StatusBadGateway || bytes.Contains(body, []byte(mockMarker)) {
			t.Errorf("fail-closed degraded after hang storm: status %d", resp.StatusCode)
		}
	})

}

//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

const mockMarker = "MOCK-SECRET-BYTES"

// mockHTTP dials the proxy-on-mock's unix listen socket.
var mockHTTP *http.Client

// setupMockProxy builds the mockdaemon image, runs it with its unix socket
// in the shared socket dir, and starts a second proxy instance with
// SOCKET_PATH pointed at the mock and its own listen socket alongside.
// Returns the base URL for mockHTTP.
func setupMockProxy(t *testing.T) string {
	t.Helper()

	image := "bsp-e2e-mockdaemon:" + nonce
	if err := buildScratchImage("./mockdaemon", image); err != nil {
		t.Fatalf("build mockdaemon image: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/images/"+image+"?force=true", nil, nil, 200)
	})

	// Mock daemon and proxy-on-mock share the same socket dir as the main
	// harness; distinct filenames keep them apart.
	const mockSock = proxySockDir + "/mock.sock"
	const listenSock = proxySockDir + "/proxy.sock"

	mockID, err := startContainer("bsp-e2e-mockdaemon-"+nonce, map[string]any{
		"Image": image,
		"Env":   []string{"MOCK_SOCKET=" + mockSock},
		"HostConfig": map[string]any{
			"Binds": []string{sockMount},
		},
	})
	if err != nil {
		t.Fatalf("mockdaemon container: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/containers/"+mockID+"?force=true", nil, nil, 204)
	})

	proxyID, err := startContainer("bsp-e2e-proxymock-"+nonce, map[string]any{
		"Image": os.Getenv("PROXY_IMAGE"),
		"Env": []string{
			"SOCKET_PATH=" + mockSock,
			"LISTEN_ADDR=" + listenSock,
			"LISTEN_SOCKET_MODE=0666",
		},
		"HostConfig": map[string]any{
			"Binds":          []string{sockMount},
			"CapDrop":        []string{"ALL"},
			"ReadonlyRootfs": true,
			"SecurityOpt":    []string{"no-new-privileges:true"},
		},
	})
	if err != nil {
		t.Fatalf("proxy-on-mock container: %v", err)
	}
	t.Cleanup(func() {
		_ = docker.call("DELETE", "/containers/"+proxyID+"?force=true", nil, nil, 204)
	})

	base := "http://proxy"
	mockHTTP = unixClient(dialPath("proxy.sock"), 60*time.Second)
	if err := waitFor("proxy-on-mock ready", func() error {
		resp, err := mockHTTP.Get(base + "/version")
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

func mockGet(t *testing.T, base, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := mockHTTP.Do(req)
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
		if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("GET %s: 502 Content-Type = %q, want application/json", path, ct)
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
			resp, err := mockHTTP.Get(base + "/containers/hang/json")
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
		impatient := unixClient(dialPath("proxy.sock"), 2*time.Second)
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
			resp, err := mockHTTP.Get(base + "/version")
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

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestVersion(t *testing.T) {
	pResp, pBody := proxyDo(t, http.MethodGet, "/version")
	dResp, dBody := directDo(t, http.MethodGet, "/version")

	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", pResp.StatusCode)
	}
	if !bytes.Equal(pBody, dBody) {
		t.Errorf("body differs from direct socket:\nproxy:  %s\ndirect: %s", pBody, dBody)
	}
	// Beszel reads the Server header for Podman detection; both headers
	// must pass through unmodified.
	for _, h := range []string{"Server", "Api-Version"} {
		if got, want := pResp.Header.Get(h), dResp.Header.Get(h); got != want || got == "" {
			t.Errorf("%s header = %q, want %q (non-empty)", h, got, want)
		}
	}
}

func TestInfo(t *testing.T) {
	pResp, pBody := proxyDo(t, http.MethodGet, "/info")
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", pResp.StatusCode)
	}
	_, dBody := directDo(t, http.MethodGet, "/info")

	pInfo := parseJSON(t, pBody).(map[string]any)
	dInfo := parseJSON(t, dBody).(map[string]any)
	for _, key := range []string{"NCPU", "MemTotal", "OperatingSystem", "KernelVersion"} {
		if pInfo[key] == nil || !reflect.DeepEqual(pInfo[key], dInfo[key]) {
			t.Errorf("%s = %v, want %v", key, pInfo[key], dInfo[key])
		}
	}
}

// normalizeList makes a container list comparable across two separate calls.
// It drops the one volatile-valued field ("Status": "Up 5 seconds" ->
// "Up 6 seconds") and canonicalizes the rest: Docker does not guarantee the
// order of the top-level list nor of nested arrays (e.g. each entry's
// Mounts), so those orderings differ between the proxied and direct calls
// even though the content is identical.
func normalizeList(list []any) any {
	for _, c := range list {
		delete(c.(map[string]any), "Status")
	}
	return canonicalize(list)
}

// canonicalize recursively sorts every array by its JSON encoding so that
// order-insensitive structures compare equal. Object keys are already
// canonical (encoding/json sorts map keys).
func canonicalize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = canonicalize(e)
		}
		return x
	case []any:
		for i := range x {
			x[i] = canonicalize(x[i])
		}
		sort.Slice(x, func(i, j int) bool {
			bi, _ := json.Marshal(x[i])
			bj, _ := json.Marshal(x[j])
			return string(bi) < string(bj)
		})
		return x
	default:
		return v
	}
}

func TestContainerList(t *testing.T) {
	pResp, pBody := proxyDo(t, http.MethodGet, "/containers/json")
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", pResp.StatusCode)
	}
	_, dBody := directDo(t, http.MethodGet, "/containers/json")

	pList := normalizeList(parseJSON(t, pBody).([]any))
	dList := normalizeList(parseJSON(t, dBody).([]any))
	// Identical (normalized) payload proves the list is not routed
	// through the inspect filter.
	if !reflect.DeepEqual(pList, dList) {
		t.Errorf("container list differs from direct socket:\nproxy:  %s\ndirect: %s", pBody, dBody)
	}

	for _, want := range []string{envCtr.name, ttyCtr.name} {
		if !bytes.Contains(pBody, []byte(want)) {
			t.Errorf("fixture %s missing from container list", want)
		}
	}
}

func TestStats(t *testing.T) {
	refs := map[string]string{
		"full ID":  envCtr.id,
		"short ID": envCtr.id[:12],
		"name":     envCtr.name,
	}
	for label, ref := range refs {
		t.Run(label, func(t *testing.T) {
			resp, body := proxyDo(t, http.MethodGet, "/containers/"+ref+"/stats?stream=0&one-shot=1")
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
			}
			stats := parseJSON(t, body).(map[string]any)
			for _, key := range []string{"cpu_stats", "memory_stats", "networks"} {
				if stats[key] == nil {
					t.Errorf("%s missing from stats response", key)
				}
			}
		})
	}

	// Beszel fetches stats for all containers concurrently (semaphore
	// width 5); the proxy must serve that pattern.
	t.Run("5 concurrent", func(t *testing.T) {
		var wg sync.WaitGroup
		errs := make(chan error, 5)
		for range 5 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, _ := http.NewRequest(http.MethodGet, proxyBase+"/containers/"+envCtr.id+"/stats?stream=0&one-shot=1", nil)
				resp, err := proxyHTTP.Do(req)
				if err != nil {
					errs <- err
					return
				}
				defer func() { _ = resp.Body.Close() }()
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Errorf("status %d", resp.StatusCode)
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Errorf("concurrent stats: %v", err)
		}
	})
}

func TestInspect(t *testing.T) {
	pResp, pBody := proxyDo(t, http.MethodGet, "/containers/"+envCtr.id+"/json")
	if pResp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", pResp.StatusCode)
	}
	_, dBody := directDo(t, http.MethodGet, "/containers/"+envCtr.id+"/json")

	// The oracle proves the sentinel exists at the source...
	if !bytes.Contains(dBody, []byte(secret)) {
		t.Fatalf("sentinel %s missing from direct inspect; fixture broken", secret)
	}
	// ...and must appear nowhere in the proxied bytes.
	if bytes.Contains(pBody, []byte(secret)) {
		t.Fatalf("sentinel env value leaked through proxy")
	}
	if bytes.Contains(pBody, []byte("E2E_SECRET")) {
		t.Fatalf("env var name leaked through proxy")
	}

	pDoc := parseJSON(t, pBody).(map[string]any)
	dDoc := parseJSON(t, dBody).(map[string]any)
	pConfig := pDoc["Config"].(map[string]any)
	if _, ok := pConfig["Env"]; ok {
		t.Errorf("Config.Env present in proxied inspect")
	}
	// Deep-equal to direct after del(.Config.Env): nothing else may change.
	// Canonicalize first — the inspect doc also carries nondeterministically
	// ordered arrays (e.g. Mounts) that differ between the two calls.
	delete(dDoc["Config"].(map[string]any), "Env")
	if !reflect.DeepEqual(canonicalize(pDoc), canonicalize(dDoc)) {
		t.Errorf("inspect diverges from direct beyond Env removal:\nproxy:  %s\ndirect: %s", pBody, dBody)
	}

	t.Run("ID forms", func(t *testing.T) {
		for label, ref := range map[string]string{
			"short ID": envCtr.id[:12],
			"name":     envCtr.name,
		} {
			resp, body := proxyDo(t, http.MethodGet, "/containers/"+ref+"/json")
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s: status = %d, want 200", label, resp.StatusCode)
				continue
			}
			if bytes.Contains(body, []byte(secret)) {
				t.Errorf("%s: sentinel leaked", label)
			}
		}
	})

	t.Run("404 passthrough", func(t *testing.T) {
		missing := "/containers/no-such-" + nonce + "/json"
		pResp, pBody := proxyDo(t, http.MethodGet, missing)
		dResp, dBody := directDo(t, http.MethodGet, missing)
		if pResp.StatusCode != http.StatusNotFound || dResp.StatusCode != http.StatusNotFound {
			t.Fatalf("status proxy=%d direct=%d, want 404", pResp.StatusCode, dResp.StatusCode)
		}
		if !bytes.Equal(pBody, dBody) {
			t.Errorf("404 body differs:\nproxy:  %s\ndirect: %s", pBody, dBody)
		}
	})
}

func TestLogs(t *testing.T) {
	cases := []struct {
		label       string
		ctr         fixture
		contentType string
	}{
		{"multiplexed (tty=false)", envCtr, "application/vnd.docker.multiplexed-stream"},
		{"raw (tty=true)", ttyCtr, "application/vnd.docker.raw-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			path := "/containers/" + tc.ctr.id + "/logs?stdout=1&stderr=1"
			pResp, pBody := proxyDo(t, http.MethodGet, path)
			if pResp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", pResp.StatusCode)
			}
			dBody, dType, err := directLogs(tc.ctr.id, "stdout=1&stderr=1")
			if err != nil {
				t.Fatalf("direct logs: %v", err)
			}
			if !bytes.Equal(pBody, dBody) {
				t.Errorf("log payload differs from direct socket:\nproxy:  %q\ndirect: %q", pBody, dBody)
			}
			if got := pResp.Header.Get("Content-Type"); got != tc.contentType || got != dType {
				t.Errorf("Content-Type = %q, want %q (direct: %q)", got, tc.contentType, dType)
			}
		})
	}

	// Stream selection: stdout=1 without stderr must yield exactly the
	// stdout lines (stream filtering is per-stream, so this is
	// deterministic regardless of stdout/stderr interleaving).
	t.Run("stream selection", func(t *testing.T) {
		path := "/containers/" + envCtr.id + "/logs?stdout=1"
		pResp, pBody := proxyDo(t, http.MethodGet, path)
		if pResp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", pResp.StatusCode)
		}
		dBody, _, err := directLogs(envCtr.id, "stdout=1")
		if err != nil {
			t.Fatalf("direct logs: %v", err)
		}
		if !bytes.Equal(pBody, dBody) {
			t.Errorf("stdout-only payload differs:\nproxy:  %q\ndirect: %q", pBody, dBody)
		}
		body := string(pBody)
		for _, want := range []string{"e2e-stdout-one", "e2e-stdout-two"} {
			if !strings.Contains(body, want) {
				t.Errorf("stdout log missing %q: %q", want, body)
			}
		}
		if strings.Contains(body, "e2e-stderr-one") {
			t.Errorf("stderr line present despite stdout=1 only: %q", body)
		}
	})

	// Tail: the daemon applies tail before stream filtering, so assert
	// only that the param reaches the daemon (response shrinks) and the
	// payload matches the direct socket byte for byte.
	t.Run("tail honored", func(t *testing.T) {
		full := "/containers/" + envCtr.id + "/logs?stdout=1&stderr=1"
		_, fullBody := proxyDo(t, http.MethodGet, full)

		path := full + "&tail=1"
		pResp, pBody := proxyDo(t, http.MethodGet, path)
		if pResp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", pResp.StatusCode)
		}
		dBody, _, err := directLogs(envCtr.id, "stdout=1&stderr=1&tail=1")
		if err != nil {
			t.Fatalf("direct logs: %v", err)
		}
		if !bytes.Equal(pBody, dBody) {
			t.Errorf("tail=1 payload differs:\nproxy:  %q\ndirect: %q", pBody, dBody)
		}
		if len(pBody) == 0 || len(pBody) >= len(fullBody) {
			t.Errorf("tail=1 not honored: %d bytes vs full %d bytes", len(pBody), len(fullBody))
		}
	})
}

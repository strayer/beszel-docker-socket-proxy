package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// goldenInspect is a trimmed-down container inspect response: Config.Env
// carrying a sentinel secret, plus representative sibling keys at every
// level that the filter must leave intact.
const goldenInspect = `{
	"Id": "abc123",
	"Name": "/e2e-env",
	"State": {"Status": "running", "Health": {"Status": "healthy"}},
	"Config": {
		"Env": ["SECRET_TOKEN=leakcheck-sentinel", "PATH=/usr/bin"],
		"Image": "alpine",
		"Labels": {"com.example.label": "value"},
		"Cmd": ["sleep", "infinity"],
		"ExposedPorts": {}
	},
	"Mounts": [],
	"NetworkSettings": {"Networks": {"bridge": {"IPAddress": "172.17.0.2"}}}
}`

func inspectResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestStripEnvRemovesEnvKeepsSiblings(t *testing.T) {
	resp := inspectResponse(http.StatusOK, goldenInspect)
	if err := stripEnvFromInspect(resp); err != nil {
		t.Fatalf("stripEnvFromInspect: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read filtered body: %v", err)
	}
	if bytes.Contains(body, []byte("leakcheck-sentinel")) {
		t.Fatalf("sentinel env value still present in filtered body: %s", body)
	}

	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("filtered body is not valid JSON: %v", err)
	}
	config, ok := got["Config"].(map[string]any)
	if !ok {
		t.Fatalf("Config missing from filtered body: %s", body)
	}
	if _, exists := config["Env"]; exists {
		t.Fatalf("Config.Env still present: %v", config["Env"])
	}

	// Everything except Config.Env must deep-equal the original.
	var want map[string]any
	if err := json.Unmarshal([]byte(goldenInspect), &want); err != nil {
		t.Fatalf("parse golden: %v", err)
	}
	delete(want["Config"].(map[string]any), "Env")
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("filtered body diverges beyond Env removal:\nwant %s\ngot  %s", wantJSON, gotJSON)
	}

	// Content-Length must match the rewritten body.
	if resp.ContentLength != int64(len(body)) {
		t.Fatalf("ContentLength %d != body length %d", resp.ContentLength, len(body))
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length header %q != body length %d", cl, len(body))
	}
}

func TestStripEnvMalformedJSON(t *testing.T) {
	resp := inspectResponse(http.StatusOK, `{"Config": {"Env": ["SECRET=x"]`) // truncated
	if err := stripEnvFromInspect(resp); err == nil {
		t.Fatal("expected error for malformed JSON, got nil")
	}
}

func TestStripEnvOversizeBody(t *testing.T) {
	huge := `{"Config":{"Env":["SECRET=x"]},"Pad":"` + strings.Repeat("x", maxInspectBody) + `"}`
	resp := inspectResponse(http.StatusOK, huge)
	if err := stripEnvFromInspect(resp); err == nil {
		t.Fatal("expected error for oversize body, got nil")
	}
}

func TestStripEnvNonOKUntouched(t *testing.T) {
	const errBody = `{"message":"No such container: nope"}`
	resp := inspectResponse(http.StatusNotFound, errBody)
	if err := stripEnvFromInspect(resp); err != nil {
		t.Fatalf("stripEnvFromInspect on 404: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != errBody {
		t.Fatalf("404 body modified: %s", body)
	}
}

// startUpstream serves handler on a unix socket in a temp dir and returns
// the socket path, standing in for the Docker daemon.
func startUpstream(t *testing.T, handler http.Handler) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen on %s: %v", sock, err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func doProxied(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

// TestFailClosed asserts the central security property at the HTTP layer:
// when the inspect filter fails, the client gets a 502 and not a single
// byte of the original (secret-bearing) upstream body.
func TestFailClosed(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"invalid JSON", `SECRET-BYTES this is not json SECRET-BYTES`},
		{"oversize body", `{"Config":{"Env":["SECRET-BYTES=1"]},"Pad":"` + strings.Repeat("y", maxInspectBody) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sock := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			h := NewHandler(sock)

			rec := doProxied(t, h, http.MethodGet, "/containers/abc123/json")
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want 502", rec.Code)
			}
			if bytes.Contains(rec.Body.Bytes(), []byte("SECRET-BYTES")) {
				t.Fatalf("upstream body leaked through failed filter: %s", rec.Body.String())
			}
		})
	}
}

// TestInspectFiltered runs the golden inspect through the full handler.
func TestInspectFiltered(t *testing.T) {
	sock := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, goldenInspect)
	}))
	h := NewHandler(sock)

	rec := doProxied(t, h, http.MethodGet, "/containers/abc123/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("leakcheck-sentinel")) {
		t.Fatalf("sentinel leaked: %s", rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("proxied body not JSON: %v", err)
	}
	if got["Name"] != "/e2e-env" {
		t.Fatalf("sibling key lost, Name = %v", got["Name"])
	}
}

// TestInspectErrorPassthrough: non-200 upstream responses (e.g. 404 for an
// unknown container) pass through unfiltered.
func TestInspectErrorPassthrough(t *testing.T) {
	const errBody = `{"message":"No such container: nope"}`
	sock := startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, errBody)
	}))
	h := NewHandler(sock)

	rec := doProxied(t, h, http.MethodGet, "/containers/nope/json")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if rec.Body.String() != errBody {
		t.Fatalf("404 body modified: %s", rec.Body.String())
	}
}

// upstreamMarker is what the fake daemon answers on every path; seeing it
// in a response proves the request reached upstream.
const upstreamMarker = `{"upstream":true}`

func markerUpstream(t *testing.T) string {
	return startUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamMarker)
	}))
}

func TestRoutingAllowed(t *testing.T) {
	h := NewHandler(markerUpstream(t))
	allowed := []string{
		"/containers/json",
		"/containers/abc123/stats",
		"/containers/abc123/stats?stream=0&one-shot=1",
		"/containers/abc123/json",
		"/containers/abc123/logs",
		"/containers/my-container.1_x/logs",
		"/version",
		"/info",
	}
	for _, path := range allowed {
		rec := doProxied(t, h, http.MethodGet, path)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, rec.Code)
			continue
		}
		if !strings.Contains(rec.Body.String(), `"upstream"`) {
			t.Errorf("GET %s did not reach upstream: %s", path, rec.Body.String())
		}
	}
}

func TestRoutingDenied(t *testing.T) {
	h := NewHandler(markerUpstream(t))

	denied := []struct {
		method string
		path   string
	}{
		// representative read paths outside the allowlist
		{http.MethodGet, "/images/json"},
		{http.MethodGet, "/networks"},
		{http.MethodGet, "/volumes"},
		{http.MethodGet, "/events"},
		{http.MethodGet, "/_ping"},
		{http.MethodGet, "/system/df"},
		{http.MethodGet, "/secrets"},
		{http.MethodGet, "/swarm"},
		{http.MethodGet, "/containers/abc123/top"},
		{http.MethodGet, "/containers/abc123/export"},
		{http.MethodGet, "/containers/abc123/changes"},
		{http.MethodGet, "/containers/abc123/attach"},
		// versioned prefixes
		{http.MethodGet, "/v1.54/containers/json"},
		{http.MethodGet, "/v1.54/version"},
		// mutations
		{http.MethodPost, "/containers/create"},
		{http.MethodPost, "/containers/abc123/stop"},
		{http.MethodPost, "/containers/abc123/restart"},
		{http.MethodPost, "/containers/abc123/kill"},
		{http.MethodDelete, "/containers/abc123"},
		{http.MethodPost, "/exec"},
		{http.MethodPost, "/build"},
		// method abuse on allowed paths
		{http.MethodPost, "/containers/json"},
		{http.MethodPut, "/containers/json"},
		{http.MethodDelete, "/containers/json"},
		{http.MethodPatch, "/containers/json"},
		{http.MethodPost, "/containers/abc123/stats"},
		{http.MethodPost, "/containers/abc123/json"},
		{http.MethodPost, "/containers/abc123/logs"},
		{http.MethodPost, "/version"},
		{http.MethodPost, "/info"},
		// trailing additions
		{http.MethodGet, "/containers/json/extra"},
		{http.MethodGet, "/containers/abc123/json/extra"},
	}
	for _, tc := range denied {
		rec := doProxied(t, h, tc.method, tc.path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), `"upstream"`) {
			t.Errorf("%s %s reached upstream despite deny", tc.method, tc.path)
		}
	}
}

// TestValidateIDBadCharset: an {id} segment outside the container ID/name
// charset is denied before it can reach the daemon.
func TestValidateIDBadCharset(t *testing.T) {
	h := NewHandler(markerUpstream(t))
	for _, path := range []string{
		"/containers/a%20b/json",  // space
		"/containers/a%00b/json",  // NUL
		"/containers/a$(x)b/json", // shell metachars
	} {
		rec := doProxied(t, h, http.MethodGet, path)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, rec.Code)
		}
	}
}

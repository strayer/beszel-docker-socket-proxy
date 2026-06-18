//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// inspectState fetches the fields used to detect mutation side effects:
// a successful stop/kill flips Running, a restart changes StartedAt.
func inspectState(t *testing.T, id string) (running bool, startedAt string) {
	t.Helper()
	var doc struct {
		State struct {
			Running   bool
			StartedAt string
		}
	}
	if err := docker.call("GET", "/containers/"+id+"/json", nil, &doc, 200); err != nil {
		t.Fatalf("inspect %s: %v", id, err)
	}
	return doc.State.Running, doc.State.StartedAt
}

func TestDenyMatrix(t *testing.T) {
	_, startedAtBefore := inspectState(t, envCtr.id)
	deniedName := "bsp-e2e-denied-" + nonce

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
		{http.MethodGet, "/containers/" + envCtr.id + "/top"},
		{http.MethodGet, "/containers/" + envCtr.id + "/export"},
		{http.MethodGet, "/containers/" + envCtr.id + "/changes"},
		{http.MethodGet, "/containers/" + envCtr.id + "/attach"},
		// versioned prefixes bypass nothing
		{http.MethodGet, "/v1.54/containers/json"},
		{http.MethodGet, "/v1.54/version"},
		// mutation attempts
		{http.MethodPost, "/containers/create?name=" + deniedName},
		{http.MethodPost, "/containers/" + envCtr.id + "/stop"},
		{http.MethodPost, "/containers/" + envCtr.id + "/restart"},
		{http.MethodPost, "/containers/" + envCtr.id + "/kill"},
		{http.MethodDelete, "/containers/" + envCtr.id},
		{http.MethodPost, "/containers/" + envCtr.id + "/exec"},
		{http.MethodPost, "/exec"},
		{http.MethodPost, "/build"},
		// method abuse on every allowed path
		{http.MethodPost, "/containers/json"},
		{http.MethodPut, "/containers/json"},
		{http.MethodDelete, "/containers/json"},
		{http.MethodPatch, "/containers/json"},
		{http.MethodPost, "/containers/" + envCtr.id + "/stats"},
		{http.MethodPost, "/containers/" + envCtr.id + "/json"},
		{http.MethodPost, "/containers/" + envCtr.id + "/logs"},
		{http.MethodPost, "/version"},
		{http.MethodPost, "/info"},
		// HEAD denied: Beszel uses GET only, and ServeMux would otherwise
		// serve HEAD for every GET pattern.
		{http.MethodHead, "/version"},
		{http.MethodHead, "/containers/json"},
		{http.MethodHead, "/containers/" + envCtr.id + "/json"},
		// id charset violations
		{http.MethodGet, "/containers/a%20b/json"},
		{http.MethodGet, "/containers/a%00b/stats"},
	}
	for _, tc := range denied {
		resp, body := proxyDo(t, tc.method, tc.path)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403", tc.method, tc.path, resp.StatusCode)
			continue
		}
		if !bytes.Contains(body, []byte("forbidden")) {
			t.Errorf("%s %s: deny body not from proxy: %s", tc.method, tc.path, body)
		}
	}

	// Path shenanigans that hit ServeMux path cleaning: the proxy answers
	// with a redirect to the canonical path, never with daemon data. The
	// redirect target must itself be inside the allowlist or denied.
	t.Run("path traversal", func(t *testing.T) {
		shenanigans := []struct {
			path string
		}{
			{"//containers/json"},
			{"/containers/%2e%2e/json"},
			{"/containers/json/..%2fsecrets"},
		}
		for _, tc := range shenanigans {
			resp, body := proxyDo(t, http.MethodGet, tc.path)
			switch resp.StatusCode {
			case http.StatusForbidden:
				// denied outright: fine
			case http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
				loc := resp.Header.Get("Location")
				if leaked(body) {
					t.Errorf("GET %s: redirect carries daemon data: %s", tc.path, body)
				}
				if loc != "/containers/json" && !deniedByProxy(t, loc) {
					t.Errorf("GET %s: redirect to %q escapes the allowlist", tc.path, loc)
				}
			default:
				t.Errorf("GET %s = %d, want 403 or redirect", tc.path, resp.StatusCode)
			}
		}
	})

	// Side-effect check: every mutation above must have bounced off the
	// proxy. The fixture is still running, was not restarted, and the
	// container the create attempt named does not exist.
	running, startedAtAfter := inspectState(t, envCtr.id)
	if !running {
		t.Errorf("fixture %s no longer running after deny matrix", envCtr.name)
	}
	if startedAtAfter != startedAtBefore {
		t.Errorf("fixture %s was restarted: StartedAt %s -> %s", envCtr.name, startedAtBefore, startedAtAfter)
	}
	resp, err := docker.do("GET", "/containers/"+deniedName+"/json", nil)
	if err != nil {
		t.Fatalf("direct inspect %s: %v", deniedName, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("container %s exists (status %d): create attempt reached the daemon", deniedName, resp.StatusCode)
	}
}

// leaked reports whether a response body carries anything that looks like
// daemon data rather than a proxy deny/redirect stub.
func leaked(body []byte) bool {
	return bytes.Contains(body, []byte(secret)) ||
		bytes.Contains(body, []byte(nonce)) ||
		bytes.Contains(body, []byte(`"Containers"`))
}

// deniedByProxy follows a redirect Location once and reports whether the
// proxy denies it.
func deniedByProxy(t *testing.T, loc string) bool {
	t.Helper()
	if loc == "" || !strings.HasPrefix(loc, "/") {
		return false
	}
	resp, _ := proxyDo(t, http.MethodGet, loc)
	return resp.StatusCode == http.StatusForbidden
}

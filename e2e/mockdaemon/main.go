// mockdaemon is a stand-in Docker daemon for the fail-closed e2e tests.
// It serves canned responses on a unix socket; the container "id" in the
// request path selects the failure mode.
package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"strings"
)

const marker = "MOCK-SECRET-BYTES"

func main() {
	sock := os.Getenv("MOCK_SOCKET")
	if sock == "" {
		sock = "/shared/mock.sock"
	}
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		log.Fatal(err)
	}

	// A valid JSON document just over the proxy's 8 MiB inspect limit.
	huge := `{"Marker":"` + marker + `","Pad":"` + strings.Repeat("x", 9<<20) + `"}`

	mux := http.NewServeMux()
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Version":"mockdaemon","ApiVersion":"1.54"}`))
	})
	// Invalid JSON carrying the leak marker: the proxy must answer 502
	// and forward none of it.
	mux.HandleFunc("GET /containers/badjson/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Config": ` + marker + ` this is not json`))
	})
	// Valid JSON exceeding the proxy's size limit: 502, no leak.
	mux.HandleFunc("GET /containers/huge/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(huge))
	})
	// Daemon-side error: error bodies are not filtered and must pass
	// through untouched.
	mux.HandleFunc("GET /containers/error500/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"mockdaemon: simulated daemon failure"}`))
	})
	// Hung upstream: never respond; release the goroutine when the
	// proxy gives up and cancels.
	mux.HandleFunc("GET /containers/hang/json", func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	log.Printf("mockdaemon listening on %s", sock)
	log.Fatal((&http.Server{Handler: mux}).Serve(ln))
}

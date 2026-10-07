package pritunl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A Pritunl web-server restart answers concurrent callers with transport
// errors or 5xx for a few seconds; the transport rides it out, signing
// every attempt with a fresh nonce.
func TestRoundTripRetriesTransientFailures(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	nonces := map[string]bool{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		nonces[r.Header.Get("Auth-Nonce")] = true
		failing := attempts <= 2
		mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	httpClient := &http.Client{Transport: &transport{
		baseUrl:             server.URL,
		apiToken:            "token",
		apiSecret:           "secret",
		underlyingTransport: http.DefaultTransport,
	}}

	req, err := http.NewRequest("PUT", "/settings", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the retried request to succeed, got %d", resp.StatusCode)
	}
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
	if len(nonces) != 3 {
		t.Fatalf("every attempt must sign with a fresh nonce, got %d distinct", len(nonces))
	}
}

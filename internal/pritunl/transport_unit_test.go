package pritunl

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// A POST that reached the server may have committed before the answer was
// lost; a 5xx answer therefore never replays it.
func TestRoundTripNeverRetriesDeliveredPosts(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	httpClient := &http.Client{Transport: &transport{
		baseUrl:             server.URL,
		apiToken:            "token",
		apiSecret:           "secret",
		underlyingTransport: http.DefaultTransport,
	}}

	req, err := http.NewRequest("POST", "/organization", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected the 503 to surface, got %d", resp.StatusCode)
	}
	if attempts != 1 {
		t.Fatalf("a delivered POST must never replay, got %d attempts", attempts)
	}
}

// A POST whose connection never established cannot have committed anything
// server side, so it retries like every other method.
func TestRoundTripRetriesUndeliveredPosts(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close() // nothing listens: dial fails until the server starts

	attempts := int32(0)
	go func() {
		time.Sleep(1500 * time.Millisecond)
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return
		}
		server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusOK)
		})}
		server.Serve(l)
	}()

	httpClient := &http.Client{Transport: &transport{
		baseUrl:             "http://" + addr,
		apiToken:            "token",
		apiSecret:           "secret",
		underlyingTransport: http.DefaultTransport,
	}}

	req, err := http.NewRequest("POST", "/organization", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected the undelivered POST to retry into a 200, got %d", resp.StatusCode)
	}
	if atomic.LoadInt32(&attempts) != 1 {
		t.Fatalf("exactly one delivered attempt expected, got %d", attempts)
	}
}

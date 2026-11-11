package pritunl

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The restart view of the client makes one attempt per call, on a new
// connection each time, and reports a refused answer as an ApiError the
// caller can classify.
func TestForRestartIsSingleAttemptOnFreshConnections(t *testing.T) {
	var requests, conns int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&conns, 1)
		}
	}
	server.Start()
	defer server.Close()

	restart := NewClient(server.URL, "token", "secret", false).(RestartScoper).ForRestart(context.Background())

	err := restart.StartServer("srv")
	if !IsTransient(err) {
		t.Fatalf("a 503 must surface as a transient error after one attempt, got %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("one attempt expected, the transport made %d", got)
	}

	if err := restart.StartServer("srv"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := atomic.LoadInt32(&conns); got != 2 {
		t.Errorf("every call must open its own connection, got %d", got)
	}
}

func TestForRestartHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()

	err := NewClient(server.URL, "t", "s", false).(RestartScoper).ForRestart(ctx).StartServer("srv")
	if err == nil || IsTransient(err) {
		t.Fatalf("a cancelled context must fail and not be transient, got %v", err)
	}
}

// A body cut off after the headers is a transient failure, not an empty
// route list.
func TestGetRoutesByServerCutOffBodyIsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		w.Write([]byte("[{"))
	}))
	defer server.Close()

	routes, err := NewClient(server.URL, "t", "s", false).(RestartScoper).ForRestart(context.Background()).GetRoutesByServer("srv")
	if err == nil || !IsTransient(err) || routes != nil {
		t.Fatalf("want a transient error and no routes, got %v, %v", routes, err)
	}
}

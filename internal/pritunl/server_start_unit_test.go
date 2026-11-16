package pritunl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// startingInstance is a Pritunl the start of a server is scripted on: every
// start request takes the next answer of starts, and every read the next
// state of reads (the last one repeats).
type startingInstance struct {
	Client

	starts []error
	reads  []Server

	startCalls int
	readCalls  int
}

func (f *startingInstance) StartServerContext(context.Context, string) error {
	f.startCalls++
	if len(f.starts) == 0 {
		return nil
	}
	err := f.starts[0]
	f.starts = f.starts[1:]
	return err
}

func (f *startingInstance) GetServerContext(_ context.Context, id string) (*Server, error) {
	f.readCalls++
	server := f.reads[0]
	if len(f.reads) > 1 {
		f.reads = f.reads[1:]
	}
	server.ID = id
	return &server, nil
}

func shortenServerStart(t *testing.T) {
	t.Helper()
	interval, settle := serverStartPollInterval, serverStartSettle
	serverStartPollInterval, serverStartSettle = time.Millisecond, 40*time.Second
	t.Cleanup(func() { serverStartPollInterval, serverStartSettle = interval, settle })
}

func online(uptime int) Server {
	return Server{Name: "vpn-a", Status: ServerStatusOnline, Uptime: uptime}
}

var (
	offline = Server{Name: "vpn-a", Status: ServerStatusOffline}
	pending = Server{Name: "vpn-a", Status: ServerStatusPending}
	// the answer a Pritunl that cannot start the server gives through
	// pritunl-web, "Server start timed out" included
	startTimedOut = newApiError("starting the server", http.StatusInternalServerError, []byte("500: Internal Server Error"))
	webRestarting = newApiError("starting the server", http.StatusBadGateway, nil)
)

func TestStartServerOnlineStartAnsweredThenOnline(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{offline, online(1)}}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 1 || instance.readCalls != 2 {
		t.Fatalf("one start and two reads expected, got %d starts and %d reads", instance.startCalls, instance.readCalls)
	}
}

// The server stops right after a start that was answered 200 ("Startup doc
// lost, stopping server"): it is started again.
func TestStartServerOnlineStartedThenStoppedIsStartedAgain(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{offline, offline, online(1)}}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 2 {
		t.Fatalf("the stopped server must be started again, got %d starts", instance.startCalls)
	}
}

// Starts that fail transiently are sent again once the server reads offline;
// after them the server is confirmed by the settle time, not by the 200 that
// finally comes.
func TestStartServerOnlineTransientFailuresThenStarted(t *testing.T) {
	shortenServerStart(t)
	dropped := &syscallError{syscall.ECONNRESET}
	instance := &startingInstance{
		starts: []error{webRestarting, dropped, startTimedOut, nil},
		reads:  []Server{offline, offline, offline, offline, online(1), online(20), online(40)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 4 || instance.readCalls != 7 {
		t.Fatalf("4 starts and 7 reads expected, got %d and %d", instance.startCalls, instance.readCalls)
	}
}

// The production failure: the answer of a start is lost while Pritunl is
// still running it (the server already reads online), and Pritunl then gives
// the start up and puts the server back offline. A 200 to a start sent while
// the first may still run proves nothing, so online is only trusted once it
// held past the settle time; until then the revert is waited for and the
// server started again, and confirmed by the settle time too.
func TestStartServerOnlineLostAnswerWaitsOutTheRevert(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{webRestarting, nil},
		reads:  []Server{offline, online(3), online(10), online(28), offline, online(1), online(41)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 2 {
		t.Fatalf("the reverted start must be sent again, got %d starts", instance.startCalls)
	}
	if instance.readCalls != 7 {
		t.Fatalf("online must hold past the settle time, got %d reads", instance.readCalls)
	}
}

// The lost request reaches Pritunl only after the server read offline: the
// retry is answered 200 without doing anything while the lost start runs, and
// that start then times out. The 200 confirms nothing; the revert is seen and
// the server started again.
func TestStartServerOnlineLostRequestBeginsAfterTheOfflineRead(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{webRestarting, nil, nil},
		reads:  []Server{offline, offline, online(1), online(15), offline, online(1), online(40)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 3 || instance.readCalls != 7 {
		t.Fatalf("3 starts and 7 reads expected, got %d and %d", instance.startCalls, instance.readCalls)
	}
}

// After a lost answer, a server that stays online past the settle time is the
// start that went through.
func TestStartServerOnlineLostAnswerThatWentThrough(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{webRestarting},
		reads:  []Server{offline, online(1), online(20), online(40)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 1 {
		t.Fatalf("a start that went through must not be sent again, got %d starts", instance.startCalls)
	}
}

// A pending server is never sent a start: Pritunl answers it 200 without
// starting anything and, when no generation is queued for the server, takes a
// pooled DH parameter set into memory and loses it. The server is started once
// it reads offline.
func TestStartServerOnlinePendingIsNotStartedUntilOffline(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{pending, pending, pending, offline, online(1)}}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 1 || instance.readCalls != 5 {
		t.Fatalf("one start, after the offline read, expected; got %d starts and %d reads", instance.startCalls, instance.readCalls)
	}
}

// A server that stays pending past the DH window fails with an error that
// names it and says the DH parameters were never generated; nothing was sent.
func TestStartServerOnlineDhWindowCloses(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{pending}}

	err := StartServerOnline(context.Background(), instance, "srv", time.Minute, 30*time.Millisecond)

	var dhErr *ServerDhPendingError
	if !errors.As(err, &dhErr) {
		t.Fatalf("a ServerDhPendingError expected, got %v", err)
	}
	if dhErr.ServerId != "srv" || dhErr.ServerName != "vpn-a" || dhErr.Window != 30*time.Millisecond {
		t.Fatalf("the error must name the server and the window, got %+v", dhErr)
	}
	for _, want := range []string{`server "vpn-a" (srv)`, "pending after 30ms", "never generated its DH parameters"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	if instance.startCalls != 0 {
		t.Fatalf("a pending server must not be started, got %d starts", instance.startCalls)
	}
}

// The start window begins once the server has its DH parameters: a DH wait
// longer than the start window does not use it up.
func TestStartServerOnlineStartWindowBeginsAfterTheDhWait(t *testing.T) {
	shortenServerStart(t)
	serverStartPollInterval = 5 * time.Millisecond
	reads := make([]Server, 0, 12)
	for i := 0; i < 10; i++ {
		reads = append(reads, pending)
	}
	instance := &startingInstance{reads: append(reads, offline, online(1))}

	if err := StartServerOnline(context.Background(), instance, "srv", 40*time.Millisecond, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 1 {
		t.Fatalf("one start expected, got %d", instance.startCalls)
	}
}

// A server that reads pending again after it had its DH parameters is not
// started until it reads offline.
func TestStartServerOnlinePendingAgainIsNotStarted(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{startTimedOut, nil},
		reads:  []Server{offline, pending, pending, offline, online(1)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 2 {
		t.Fatalf("2 starts expected, got %d", instance.startCalls)
	}
}

// A start Pritunl answered with "Server start timed out" is over: the server
// is started again, and the 200 that follows confirms it without waiting for
// the settle time.
func TestStartServerOnlineAnsweredTimeoutNeedsNoSettle(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{startTimedOut, nil},
		reads:  []Server{offline, offline, online(1)},
	}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 2 || instance.readCalls != 3 {
		t.Fatalf("2 starts and 3 reads expected, got %d and %d", instance.startCalls, instance.readCalls)
	}
}

func TestIsAnsweredStartFailure(t *testing.T) {
	cases := map[error]bool{
		startTimedOut: true,
		webRestarting: false,
		newApiError("starting the server", http.StatusServiceUnavailable, nil):                             false,
		newApiError("starting the server", http.StatusGatewayTimeout, nil):                                 false,
		newApiError("starting the server", http.StatusInternalServerError, []byte(`{"error": "unknown"}`)): false,
		newApiError("starting the server", http.StatusNotFound, []byte("404: Not Found")):                  false,
		&syscallError{syscall.ECONNRESET}:                                                                  false,
	}
	for err, want := range cases {
		if got := isAnsweredStartFailure(err); got != want {
			t.Errorf("%v: got %v, want %v", err, got, want)
		}
	}
}

func TestStartServerOnlineNeverOnlineTimesOut(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{
		starts: []error{startTimedOut, startTimedOut, startTimedOut, startTimedOut, startTimedOut, startTimedOut, startTimedOut, startTimedOut},
		reads:  []Server{offline},
	}

	err := StartServerOnline(context.Background(), instance, "srv", 30*time.Millisecond, time.Minute)
	var startErr *ServerStartError
	if !errors.As(err, &startErr) {
		t.Fatalf("a ServerStartError expected, got %v", err)
	}
	if startErr.ServerId != "srv" || startErr.ServerName != "vpn-a" || startErr.Status != ServerStatusOffline {
		t.Fatalf("the error must name the server and its status, got %+v", startErr)
	}
	for _, want := range []string{`server "vpn-a" (srv)`, "is not online after 30ms", "offline", "the last failure", "500"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not say %q", err, want)
		}
	}
	if instance.startCalls < 2 {
		t.Errorf("the start must be retried within the window, got %d starts", instance.startCalls)
	}
}

// A server that stays online but short of the settle time after a lost answer
// is not reported as started.
func TestStartServerOnlineUnconfirmedOnlineTimesOut(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{starts: []error{webRestarting}, reads: []Server{offline, online(5)}}

	err := StartServerOnline(context.Background(), instance, "srv", 20*time.Millisecond, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "online for 5s") {
		t.Fatalf("an unconfirmed online server must fail the call, got %v", err)
	}
}

func TestStartServerOnlineRefusalFailsAtOnce(t *testing.T) {
	shortenServerStart(t)
	refusals := []error{
		newApiError("starting the server", http.StatusNotFound, []byte("404: Not Found")),
		newApiError("starting the server", http.StatusBadRequest, []byte(`{"error": "server_missing_org", "error_msg": "Server cannot be started without any organizations."}`)),
		newApiError("starting the server", http.StatusInternalServerError, []byte(`{"error": "unknown", "error_msg": "Unknown error."}`)),
	}

	for _, refusal := range refusals {
		instance := &startingInstance{starts: []error{refusal}, reads: []Server{offline}}

		err := StartServerOnline(context.Background(), instance, "srv", time.Minute, time.Minute)
		if !errors.Is(err, refusal) {
			t.Fatalf("the refusal must be returned, got %v", err)
		}
		if instance.startCalls != 1 || instance.readCalls != 1 {
			t.Fatalf("a refusal ends the call at once, got %d starts and %d reads", instance.startCalls, instance.readCalls)
		}
	}
}

func TestStartServerOnlineHonorsContext(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{starts: []error{webRestarting}, reads: []Server{offline, online(1)}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	started := time.Now()
	err := StartServerOnline(ctx, instance, "srv", time.Hour, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the context deadline must end the call, got %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatalf("the call outlived its context")
	}
}

// The start request is sent once: replaying it while Pritunl still runs the
// first start is what gets an empty 200. Its refusal comes back as an
// ApiError.
func TestStartServerIsSingleAttempt(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Method != http.MethodPut || r.URL.Path != "/server/srv/operation/start" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	err := NewClient(server.URL, "token", "secret", false).StartServer("srv")

	var apiError *ApiError
	if !errors.As(err, &apiError) || apiError.StatusCode != http.StatusBadGateway {
		t.Fatalf("an ApiError with the 502 expected, got %v", err)
	}
	if !isTransientReconciliation(err) {
		t.Fatalf("a 502 to a start is transient")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("the start must be sent once, the transport sent it %d times", got)
	}
}

// Any other request keeps the retries of the transport.
func TestGetServerStillRetries(t *testing.T) {
	fastRetry(t)

	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"id": "srv", "name": "vpn-a", "status": "online", "uptime": 12}`))
	}))
	defer server.Close()

	got, err := NewClient(server.URL, "token", "secret", false).GetServer("srv")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ServerStatusOnline || got.Uptime != 12 {
		t.Fatalf("the server read back is wrong: %+v", got)
	}
}

// uptime is read, never written.
func TestServerMarshalJSONLeavesUptimeOut(t *testing.T) {
	data, err := (&Server{Name: "s", Uptime: 12}).MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["uptime"]; ok {
		t.Fatalf("uptime must not be sent: %s", data)
	}
}

// syscallError is a connection failure the way net reports one.
type syscallError struct{ errno syscall.Errno }

func (e *syscallError) Error() string { return "read: " + e.errno.Error() }
func (e *syscallError) Unwrap() error { return e.errno }

// A start request that never gets an answer ends when the window closes,
// instead of holding the apply.
func TestStartServerOnlineHangingRequestEndsWithTheWindow(t *testing.T) {
	shortenServerStart(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"id": "srv", "name": "vpn-a", "status": "offline"}`))
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	started := time.Now()
	err := StartServerOnline(context.Background(), NewClient(server.URL, "token", "secret", false), "srv", 100*time.Millisecond, time.Minute)

	var startErr *ServerStartError
	if !errors.As(err, &startErr) || !strings.Contains(err.Error(), "is not online after 100ms") {
		t.Fatalf("the window must end the call with a ServerStartError, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the call outlived its window: %s", elapsed)
	}
}

// A read that hangs while the server is waited for ends with the DH window.
func TestStartServerOnlineHangingReadEndsWithTheDhWindow(t *testing.T) {
	shortenServerStart(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()

	started := time.Now()
	err := StartServerOnline(context.Background(), NewClient(server.URL, "token", "secret", false), "srv", time.Minute, 100*time.Millisecond)

	var startErr *ServerStartError
	if !errors.As(err, &startErr) || !strings.Contains(err.Error(), "never read back") {
		t.Fatalf("a server never read must fail with a ServerStartError, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the call outlived its window: %s", elapsed)
	}
}

// Once the window has closed nothing more is sent, even when the pause ended
// exactly on it.
func TestStartServerOnlineSendsNothingAfterTheWindow(t *testing.T) {
	shortenServerStart(t)
	serverStartPollInterval = time.Second
	instance := &startingInstance{starts: []error{startTimedOut}, reads: []Server{offline}}

	err := StartServerOnline(context.Background(), instance, "srv", 30*time.Millisecond, time.Minute)
	if err == nil {
		t.Fatal("the call must fail")
	}
	if instance.startCalls != 1 || instance.readCalls != 2 {
		t.Fatalf("nothing may be sent after the window, got %d starts and %d reads", instance.startCalls, instance.readCalls)
	}
}

// A server that already reads online before anything was sent may be running
// a start of someone else that can still fail: it is not started (the start
// would be a no-op answered 200) and is confirmed by the settle time only.
func TestStartServerOnlineAlreadyOnlineWaitsForTheSettleTime(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{online(2), online(10), online(41)}}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 0 || instance.readCalls != 3 {
		t.Fatalf("no start and 3 reads expected, got %d starts and %d reads", instance.startCalls, instance.readCalls)
	}
}

// The start of someone else fails after the first read: the server is
// started, and confirmed by the settle time, since that start was never ours.
func TestStartServerOnlineAlreadyOnlineThenRevertedIsStarted(t *testing.T) {
	shortenServerStart(t)
	instance := &startingInstance{reads: []Server{online(2), offline, online(1), online(40)}}

	if err := StartServerOnline(context.Background(), instance, "srv", time.Second, time.Minute); err != nil {
		t.Fatal(err)
	}
	if instance.startCalls != 1 || instance.readCalls != 4 {
		t.Fatalf("one start and 4 reads expected, got %d starts and %d reads", instance.startCalls, instance.readCalls)
	}
}

// failingReads answers the first reads with servers, then fails every read.
type failingReads struct {
	startingInstance
	good int
}

func (f *failingReads) GetServerContext(ctx context.Context, id string) (*Server, error) {
	if f.readCalls >= f.good {
		f.readCalls++
		return nil, &syscallError{syscall.ECONNREFUSED}
	}
	return f.startingInstance.GetServerContext(ctx, id)
}

// A server read pending and then not read at all when the DH window closes is
// reported as unreadable, not as one that never got its DH parameters.
func TestStartServerOnlineDhWindowClosesOnAFailedRead(t *testing.T) {
	shortenServerStart(t)
	instance := &failingReads{startingInstance: startingInstance{reads: []Server{pending}}, good: 1}

	err := StartServerOnline(context.Background(), instance, "srv", time.Minute, 30*time.Millisecond)

	var dhErr *ServerDhPendingError
	if errors.As(err, &dhErr) {
		t.Fatalf("a failed last read must not be reported as a DH failure, got %v", err)
	}
	var startErr *ServerStartError
	if !errors.As(err, &startErr) || !strings.Contains(err.Error(), "could not be read back") {
		t.Fatalf("a ServerStartError about the read expected, got %v", err)
	}
	if instance.startCalls != 0 {
		t.Fatalf("nothing may be started, got %d starts", instance.startCalls)
	}
}

// A start whose answer arrived with its body cut short keeps the status of
// the answer: a refusal stays a refusal and is not taken for a start that got
// no answer, a 500 that cannot be read whole is not retried, and a 200 stays a
// 200.
func TestStartServerTruncatedBodyKeepsTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusOK} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(status)
			w.Write([]byte("cut"))
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("the test server cannot hijack the connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				conn.Close()
			}
		}))

		err := NewClient(server.URL, "token", "secret", false).StartServer("srv")
		server.Close()

		if status == http.StatusOK {
			if err != nil {
				t.Fatalf("a 200 with a body cut short is still a 200, got %v", err)
			}
			continue
		}
		var apiError *ApiError
		if !errors.As(err, &apiError) || apiError.StatusCode != status {
			t.Fatalf("an ApiError with the %d expected, got %v", status, err)
		}
		if isTransientReconciliation(err) || isAnsweredStartFailure(err) {
			t.Fatalf("a refusal cut short must stay a refusal, got %v", err)
		}
	}
}

// A 500 whose body could not be read whole ends the call at once with its
// status: it may be a refusal, and is not taken for an answered failure.
func TestStartServerOnlineTruncated500FailsAtOnce(t *testing.T) {
	shortenServerStart(t)
	cut := fatalError{newApiError("starting the server", http.StatusInternalServerError, []byte(`{"error": "unkn`))}
	instance := &startingInstance{starts: []error{cut}, reads: []Server{offline}}

	err := StartServerOnline(context.Background(), instance, "srv", time.Minute, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "the start was refused") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the truncated 500 must end the call, got %v", err)
	}
	if instance.startCalls != 1 {
		t.Fatalf("one start expected, got %d", instance.startCalls)
	}
}

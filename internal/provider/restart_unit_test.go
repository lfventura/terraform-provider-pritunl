package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// fakeServerClient scripts the answers of the calls resourceUpdateServer
// makes: errs[method] is a queue, one entry per call, and a method with an
// empty queue succeeds.
type fakeServerClient struct {
	pritunl.Client

	mu    sync.Mutex
	calls []string
	errs  map[string][]error

	// status is what GetServer reports; empty means online.
	status string
	// blockPut makes a scoped PUT wait for its context to end.
	blockPut bool
	// onPut runs on every PUT.
	onPut func()
}

// scopedFake is what ForRestart hands out: calls fail once the attempt
// context has ended, like a request bound to it.
type scopedFake struct {
	*fakeServerClient
	ctx context.Context
}

func (f *fakeServerClient) ForRestart(ctx context.Context) pritunl.Client {
	return &scopedFake{fakeServerClient: f, ctx: ctx}
}

func (s *scopedFake) UpdateServer(id string, server *pritunl.Server) error {
	if s.blockPut {
		s.call("put")
		<-s.ctx.Done()
		return fmt.Errorf("Put: %w", s.ctx.Err())
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}

	return s.fakeServerClient.UpdateServer(id, server)
}

func (s *scopedFake) StartServer(id string) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}

	return s.fakeServerClient.StartServer(id)
}

func (f *fakeServerClient) call(method string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method)
	if queue := f.errs[method]; len(queue) > 0 {
		f.errs[method] = queue[1:]
		return queue[0]
	}

	return nil
}

func (f *fakeServerClient) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0
	for _, c := range f.calls {
		if c == method {
			n++
		}
	}

	return n
}

func (f *fakeServerClient) GetServer(id string) (*pritunl.Server, error) {
	status := f.status
	if status == "" {
		status = pritunl.ServerStatusOnline
	}

	return &pritunl.Server{ID: id, Name: "s", Status: status}, nil
}
func (f *fakeServerClient) StopServer(string) error  { return f.call("stop") }
func (f *fakeServerClient) StartServer(string) error { return f.call("start") }
func (f *fakeServerClient) UpdateServer(string, *pritunl.Server) error {
	if f.onPut != nil {
		f.onPut()
	}

	return f.call("put")
}
func (f *fakeServerClient) AttachOrganizationToServer(string, string) error {
	return f.call("attach_org")
}
func (f *fakeServerClient) GetOrganizationsByServer(string) ([]pritunl.Organization, error) {
	return nil, nil
}
func (f *fakeServerClient) GetRoutesByServer(string) ([]pritunl.Route, error) { return nil, nil }
func (f *fakeServerClient) GetHostsByServer(string) ([]pritunl.Host, error)   { return nil, nil }

var (
	errTransient = &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	errGateway   = &pritunl.ApiError{Operation: "starting the server", StatusCode: 503}
	errBusiness  = &pritunl.ApiError{Operation: "updating the server", StatusCode: 400, Code: "server_not_offline", Message: "not offline"}
)

func fastBackoff(t *testing.T) {
	t.Helper()
	oldInitial, oldMax := restartBackoffInitial, restartBackoffMax
	restartBackoffInitial, restartBackoffMax = time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() { restartBackoffInitial, restartBackoffMax = oldInitial, oldMax })
}

func runServerUpdate(t *testing.T, fake *fakeServerClient, window time.Duration) error {
	t.Helper()

	return runServerUpdateWith(t, context.Background(), fake, window, nil)
}

func runServerUpdateWith(t *testing.T, ctx context.Context, fake *fakeServerClient, window time.Duration, extra map[string]interface{}) error {
	t.Helper()
	fastBackoff(t)

	raw := map[string]interface{}{
		"name":             "s",
		"organization_ids": []interface{}{"org1"},
	}
	for k, v := range extra {
		raw[k] = v
	}

	d := schema.TestResourceDataRaw(t, resourceServer().Schema, raw)
	d.SetId("srv1")

	diags := resourceUpdateServer(ctx, d, &providerMeta{Client: fake, restartRetryTimeout: window})
	if !diags.HasError() {
		return nil
	}

	msgs := make([]string, 0, len(diags))
	for _, dg := range diags {
		msgs = append(msgs, dg.Summary)
	}

	return errors.New(strings.Join(msgs, "\n"))
}

func TestUpdateServerRetriesTransientFailures(t *testing.T) {
	fake := &fakeServerClient{errs: map[string][]error{
		"attach_org": {errTransient, errTransient},
		"put":        {errTransient, errGateway, errTransient},
		"start":      {errGateway, errTransient},
	}}

	if err := runServerUpdate(t, fake, 5*time.Second); err != nil {
		t.Fatalf("update failed: %s", err)
	}

	for method, want := range map[string]int{"stop": 1, "attach_org": 3, "put": 4, "start": 3} {
		if got := fake.count(method); got != want {
			t.Errorf("%s called %d times, want %d (calls: %v)", method, got, want, fake.calls)
		}
	}
}

func TestUpdateServerDoesNotRetryBusinessErrors(t *testing.T) {
	fake := &fakeServerClient{errs: map[string][]error{"put": {errBusiness}}}

	err := runServerUpdate(t, fake, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "server_not_offline") {
		t.Fatalf("want the business error, got %v", err)
	}
	if got := fake.count("put"); got != 1 {
		t.Errorf("put called %d times, want 1", got)
	}
}

func TestUpdateServerStartRefusedIsNotRetried(t *testing.T) {
	fake := &fakeServerClient{errs: map[string][]error{"start": {errBusiness}}}

	err := runServerUpdate(t, fake, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Error on starting server") {
		t.Fatalf("want a start error, got %v", err)
	}
	if got := fake.count("start"); got != 1 {
		t.Errorf("start called %d times, want 1", got)
	}
}

func TestUpdateServerWindowExpiryStillStarts(t *testing.T) {
	always := make([]error, 10000)
	for i := range always {
		always[i] = errTransient
	}
	fake := &fakeServerClient{errs: map[string][]error{
		"put":   always,
		"start": {errTransient, errTransient, errTransient, errGateway},
	}}

	err := runServerUpdate(t, fake, 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "restart_retry_timeout") {
		t.Fatalf("want the window expiry error, got %v", err)
	}
	if fake.count("put") < 2 {
		t.Errorf("put was not retried: %v", fake.calls)
	}
	if fake.count("start") != 5 {
		t.Errorf("start called %d times after the window expired, want 5 (a window of its own): %v", fake.count("start"), fake.calls)
	}
	if last := fake.calls[len(fake.calls)-1]; last != "start" {
		t.Errorf("start must be the last call, got %v", fake.calls)
	}
}

func TestUpdateServerFailedPutStartsAndReturnsOriginalErrorFirst(t *testing.T) {
	fake := &fakeServerClient{errs: map[string][]error{"put": {errBusiness}}}

	if err := runServerUpdate(t, fake, time.Second); err == nil || !strings.Contains(err.Error(), "server_not_offline") {
		t.Fatalf("want the PUT error, got %v", err)
	}
	if fake.count("start") != 1 {
		t.Errorf("the server was not started after the failed PUT: %v", fake.calls)
	}

	both := &fakeServerClient{errs: map[string][]error{"put": {errBusiness}, "start": {errBusiness}}}
	err := runServerUpdate(t, both, time.Second)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	put, again := strings.Index(msg, "updating the server"), strings.Index(msg, "starting the server again failed too")
	if put < 0 || again < 0 || put > again {
		t.Errorf("the original error must come first, then the start failure: %q", msg)
	}
}

func TestUpdateServerStopErrorStartsWithRetry(t *testing.T) {
	fake := &fakeServerClient{errs: map[string][]error{
		"stop":  {errBusiness},
		"start": {errTransient, errGateway},
	}}

	err := runServerUpdate(t, fake, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "Error on stopping server") {
		t.Fatalf("want the stop error, got %v", err)
	}
	if fake.count("start") != 3 {
		t.Errorf("start called %d times, want 3: %v", fake.count("start"), fake.calls)
	}
	if fake.count("put") != 0 {
		t.Errorf("nothing may be applied after a failed stop: %v", fake.calls)
	}
}

func TestIsTransient(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                {nil, false},
		"dial refused":       {fmt.Errorf("x: %w", errTransient), true},
		"reset":              {fmt.Errorf("x: %w", &net.OpError{Op: "read", Err: syscall.ECONNRESET}), true},
		"eof":                {fmt.Errorf("x: %w", io.EOF), true},
		"unexpected eof":     {fmt.Errorf("x: %w", io.ErrUnexpectedEOF), true},
		"dns":                {&net.DNSError{Err: "no such host", Name: "vpn"}, true},
		"tls handshake":      {errors.New("net/http: TLS handshake timeout"), true},
		"502":                {&pritunl.ApiError{StatusCode: 502}, true},
		"503":                {&pritunl.ApiError{StatusCode: 503}, true},
		"504":                {&pritunl.ApiError{StatusCode: 504}, true},
		"500":                {&pritunl.ApiError{StatusCode: 500}, true},
		"400":                {errBusiness, false},
		"404":                {&pritunl.ApiError{StatusCode: 404}, false},
		"canceled":           {context.Canceled, false},
		"plain error":        {errors.New("boom"), false},
		"wrapped gateway":    {fmt.Errorf("a: %w", errGateway), true},
		"deadline exceeded":  {context.DeadlineExceeded, true},
		"x509 is not a flap": {&url.Error{Op: "Put", URL: "https://x", Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}, false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := pritunl.IsTransient(c.err); got != c.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestRetryStopsOnContextCancel(t *testing.T) {
	fastBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	rr := &restartRetry{ctx: ctx, window: time.Hour, deadline: time.Now().Add(time.Hour)}

	calls := 0
	err := rr.do(func(pritunl.Client) error {
		calls++
		cancel()
		return errTransient
	})
	if err == nil || calls != 1 {
		t.Fatalf("want one attempt and the error, got %d calls, err %v", calls, err)
	}
}

func TestParseRestartRetryTimeout(t *testing.T) {
	good := map[string]time.Duration{"1m": time.Minute, "90s": 90 * time.Second, "5s": 5 * time.Second}
	for in, want := range good {
		got, err := parseRestartRetryTimeout(in)
		if err != nil || got != want {
			t.Errorf("parse(%q) = %v, %v; want %v", in, got, err, want)
		}
	}

	for _, in := range []string{"", "abc", "0s", "-1m", "10"} {
		if _, err := parseRestartRetryTimeout(in); err == nil {
			t.Errorf("parse(%q) must fail", in)
		}
		if _, errs := validateRestartRetryTimeout(in, "restart_retry_timeout"); len(errs) == 0 {
			t.Errorf("validate(%q) must fail", in)
		}
	}
}

func configureTimeout(t *testing.T, raw map[string]interface{}) (time.Duration, error) {
	t.Helper()

	raw["url"], raw["token"], raw["secret"], raw["insecure"], raw["connection_check"] = "https://x", "t", "s", false, false
	p := Provider()
	diags := p.Configure(context.Background(), terraform.NewResourceConfigRaw(raw))
	if diags.HasError() {
		return 0, errors.New(diags[0].Summary)
	}

	return p.Meta().(*providerMeta).RestartRetryTimeout(), nil
}

func TestProviderRestartRetryTimeoutConfig(t *testing.T) {
	t.Setenv("PRITUNL_RESTART_RETRY_TIMEOUT", "")

	got, err := configureTimeout(t, map[string]interface{}{})
	if err != nil || got != time.Minute {
		t.Errorf("default = %v, %v; want 1m", got, err)
	}

	got, err = configureTimeout(t, map[string]interface{}{"restart_retry_timeout": "5s"})
	if err != nil || got != 5*time.Second {
		t.Errorf("argument = %v, %v; want 5s", got, err)
	}

	t.Setenv("PRITUNL_RESTART_RETRY_TIMEOUT", "2m")
	got, err = configureTimeout(t, map[string]interface{}{})
	if err != nil || got != 2*time.Minute {
		t.Errorf("env = %v, %v; want 2m", got, err)
	}

	got, err = configureTimeout(t, map[string]interface{}{"restart_retry_timeout": "10s"})
	if err != nil || got != 10*time.Second {
		t.Errorf("argument over env = %v, %v; want 10s", got, err)
	}

	t.Setenv("PRITUNL_RESTART_RETRY_TIMEOUT", "0s")
	if _, err = configureTimeout(t, map[string]interface{}{}); err == nil {
		t.Error("an env value of 0s must be refused")
	}

	diags := Provider().Validate(terraform.NewResourceConfigRaw(map[string]interface{}{
		"url": "https://x", "token": "t", "secret": "s", "insecure": false, "restart_retry_timeout": "nope",
	}))
	if !diags.HasError() {
		t.Error("an unparsable restart_retry_timeout must fail validation")
	}
}

func TestUpdateServerOfflineIsNotStarted(t *testing.T) {
	always := make([]error, 10000)
	for i := range always {
		always[i] = errTransient
	}

	cases := map[string]struct {
		fake  *fakeServerClient
		extra map[string]interface{}
	}{
		"status changed to offline, success": {
			fake:  &fakeServerClient{},
			extra: map[string]interface{}{"status": pritunl.ServerStatusOffline},
		},
		"status changed to offline, failed put": {
			fake:  &fakeServerClient{errs: map[string][]error{"put": {errBusiness}}},
			extra: map[string]interface{}{"status": pritunl.ServerStatusOffline},
		},
		"status changed to offline, stop error": {
			fake:  &fakeServerClient{errs: map[string][]error{"stop": {errBusiness}}},
			extra: map[string]interface{}{"status": pritunl.ServerStatusOffline},
		},
		"was offline, success": {
			fake: &fakeServerClient{status: pritunl.ServerStatusOffline},
		},
		"was offline, failed put": {
			fake: &fakeServerClient{status: pritunl.ServerStatusOffline, errs: map[string][]error{"put": {errBusiness}}},
		},
		"was offline, stop error": {
			fake: &fakeServerClient{status: pritunl.ServerStatusOffline, errs: map[string][]error{"stop": {errBusiness}}},
		},
		"was offline, window expired": {
			fake: &fakeServerClient{status: pritunl.ServerStatusOffline, errs: map[string][]error{"put": always}},
		},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			runServerUpdateWith(t, context.Background(), c.fake, 40*time.Millisecond, c.extra)

			if got := c.fake.count("start"); got != 0 {
				t.Errorf("start called %d times, want 0: %v", got, c.fake.calls)
			}
			if c.fake.count("stop") != 1 {
				t.Errorf("stop called %d times, want 1: %v", c.fake.count("stop"), c.fake.calls)
			}
		})
	}
}

// A call that hangs on a half-open connection ends with the window, not with
// the transport timeouts.
func TestUpdateServerAttemptIsBoundByTheWindow(t *testing.T) {
	fake := &fakeServerClient{blockPut: true}

	began := time.Now()
	err := runServerUpdate(t, fake, 150*time.Millisecond)
	elapsed := time.Since(began)

	if err == nil || !strings.Contains(err.Error(), "restart_retry_timeout") {
		t.Fatalf("want the window expiry error, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("update took %s, a hanging attempt was not cut off by the window", elapsed)
	}
	if fake.count("start") != 1 {
		t.Errorf("start called %d times, want 1: %v", fake.count("start"), fake.calls)
	}
}

// Interrupting the run while the PUT retries does not leave the server
// stopped: the start runs on a context that ignores the cancellation.
func TestUpdateServerCancelledRunStillStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeServerClient{
		errs:  map[string][]error{"put": {errTransient}, "start": {errTransient, errGateway}},
		onPut: cancel,
	}

	err := runServerUpdateWith(t, ctx, fake, 5*time.Second, nil)
	if err == nil {
		t.Fatal("want the PUT error")
	}
	if strings.Contains(err.Error(), "starting the server again failed too") {
		t.Errorf("the start failed after the cancel: %v", err)
	}
	if fake.count("start") != 3 {
		t.Errorf("start called %d times, want 3 (retried to success): %v", fake.count("start"), fake.calls)
	}
}

// The stop-error path also starts after a cancelled run.
func TestUpdateServerCancelledRunStopErrorStarts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fake := &fakeServerClient{errs: map[string][]error{"stop": {context.Canceled}, "start": {errTransient}}}

	err := runServerUpdateWith(t, ctx, fake, 5*time.Second, nil)
	if err == nil || strings.Contains(err.Error(), "starting the server again failed too") {
		t.Fatalf("want only the stop error, got %v", err)
	}
	if fake.count("start") != 2 {
		t.Errorf("start called %d times, want 2: %v", fake.count("start"), fake.calls)
	}
}

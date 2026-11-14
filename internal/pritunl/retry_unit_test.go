package pritunl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// fastRetry shortens the pauses between retries for the length of a test.
func fastRetry(t *testing.T) {
	t.Helper()

	first, max, window := transportRetryFirstWait, transportRetryMaxWait, transportRetryWindow
	transportRetryFirstWait, transportRetryMaxWait, transportRetryWindow = 5*time.Millisecond, 20*time.Millisecond, 10*time.Second
	t.Cleanup(func() { transportRetryFirstWait, transportRetryMaxWait, transportRetryWindow = first, max, window })
}

func TestIsTransientError(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"nil":                {nil, false},
		"connection refused": {&net.OpError{Op: "dial", Err: &net_syscall{syscall.ECONNREFUSED}}, true},
		"connection reset":   {fmt.Errorf("Post: %w", &net.OpError{Op: "read", Err: syscall.ECONNRESET}), true},
		"broken pipe":        {&net.OpError{Op: "write", Err: syscall.EPIPE}, true},
		"EOF":                {io.EOF, true},
		"unexpected EOF":     {io.ErrUnexpectedEOF, true},
		"i/o timeout":        {&net.OpError{Op: "read", Err: timeoutError{}}, true},
		"certificate verification": {
			&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, false,
		},
		"context canceled":  {context.Canceled, false},
		"context deadline":  {context.DeadlineExceeded, false},
		"plain error":       {errors.New("boom"), false},
		"no such host":      {&net.DNSError{Err: "no such host", IsNotFound: true}, false},
		"fatal wrapped EOF": {fatalError{io.EOF}, true},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := isTransientError(c.err); got != c.want {
				t.Fatalf("isTransientError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// net_syscall lets a bare errno sit inside an OpError the way the net package
// builds it.
type net_syscall struct{ errno syscall.Errno }

func (e *net_syscall) Error() string { return e.errno.Error() }
func (e *net_syscall) Unwrap() error { return e.errno }

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIsTransientStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{200, "", false},
		{400, `{"error": "cipher_invalid"}`, false},
		{400, "", false}, // an empty 400 is Pritunl refusing an undecodable body
		{404, "", false},
		{500, `{"error": "internal"}`, false},
		{500, "500: Internal Server Error", true},
		{500, `{"unrelated": true}`, true},
		{500, "", true},
		{500, "  \n", true},
		{502, "", true},
		{502, "Bad Gateway", true},
		{503, `{"error":"x"}`, true},
		{504, "", true},
	}

	for _, c := range cases {
		if got := isTransientStatus(c.status, []byte(c.body)); got != c.want {
			t.Errorf("isTransientStatus(%d, %q) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

func newTestHttpClient(url string) *http.Client {
	return &http.Client{Transport: &transport{
		baseUrl:             url,
		apiToken:            "token",
		apiSecret:           "secret",
		underlyingTransport: http.DefaultTransport,
	}}
}

// dropConnection ends the request the way a restarting web server does: the
// connection closes without an answer.
func dropConnection(w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		conn.Close()
	}
}

func TestRoundTripRetriesTransientAnswersAndConnectionDrops(t *testing.T) {
	fastRetry(t)

	cases := map[string]func(w http.ResponseWriter){
		"502 empty":         func(w http.ResponseWriter) { w.WriteHeader(502) },
		"503 with a body":   func(w http.ResponseWriter) { w.WriteHeader(503); w.Write([]byte("unavailable")) },
		"504 empty":         func(w http.ResponseWriter) { w.WriteHeader(504) },
		"500 empty":         func(w http.ResponseWriter) { w.WriteHeader(500) },
		"500 plain text":    func(w http.ResponseWriter) { w.WriteHeader(500); w.Write([]byte("500: Internal Server Error")) },
		"connection closed": dropConnection,
	}

	for name, failure := range cases {
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				attempts++
				failing := attempts <= 3
				mu.Unlock()
				if failing {
					failure(w)

					return
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()

			resp, err := newTestHttpClient(server.URL).Get(server.URL + "/state")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != 200 || string(body) != `{"ok":true}` {
				t.Fatalf("expected the retried request to succeed, got %d %q", resp.StatusCode, body)
			}
			if attempts != 4 {
				t.Fatalf("expected 4 attempts, got %d", attempts)
			}
		})
	}
}

// The answers of the API are the caller's to read, whatever the status.
func TestRoundTripNeverRetriesAnswersOfTheApi(t *testing.T) {
	fastRetry(t)

	cases := map[string]struct {
		status int
		body   string
	}{
		"400 with an error body": {400, `{"error": "cipher_invalid", "error_msg": "Encryption cipher is invalid."}`},
		"400 empty":              {400, ""},
		"404":                    {404, `{"error": "not_found"}`},
		"500 with an error body": {500, `{"error": "internal"}`},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer server.Close()

			resp, err := newTestHttpClient(server.URL).Get(server.URL + "/server")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != c.status || string(body) != c.body {
				t.Fatalf("the answer must reach the caller untouched, got %d %q", resp.StatusCode, body)
			}
			if attempts != 1 {
				t.Fatalf("an answer of the API must not be retried, got %d attempts", attempts)
			}
		})
	}
}

// Right after the certificate changes the client is presented one it does not
// trust, which is the web server answering and no reason to retry.
func TestRoundTripNeverRetriesCertificateVerification(t *testing.T) {
	fastRetry(t)

	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
	}))
	defer server.Close()

	_, err := newTestHttpClient(server.URL).Get(server.URL + "/state")

	var verifyErr *tls.CertificateVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("expected the certificate verification failure to surface as it is, got %v", err)
	}
	if attempts != 0 {
		t.Fatalf("nothing is sent over a connection that does not verify, got %d requests", attempts)
	}
}

// plainServer is a Server without its marshaller: Pritunl answers mss_fix as
// the number it is, and takes it as a string.
type plainServer Server

// fakeInstance is the part of a Pritunl instance the creates under test read
// and write, with a switch for each way a restart can interfere.
type fakeInstance struct {
	mu            sync.Mutex
	servers       []Server
	organizations []Organization
	routes        []Route
	users         []map[string]string
	admins        []Administrator
	posts         int

	// failures lists what happens to the first POSTs, in order: "502" answers
	// before the request is served, "drop" serves it and then loses the
	// connection, "" serves it.
	failures []string
}

func (f *fakeInstance) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		write := func(value interface{}) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(value)
		}

		if r.Method == http.MethodGet {
			switch {
			case r.URL.Path == "/server":
				plain := make([]plainServer, 0, len(f.servers))
				for _, server := range f.servers {
					plain = append(plain, plainServer(server))
				}
				write(plain)
			case strings.HasPrefix(r.URL.Path, "/server/") && strings.HasSuffix(r.URL.Path, "/route"):
				write(f.routes)
			case strings.HasPrefix(r.URL.Path, "/server/"):
				id := strings.TrimPrefix(r.URL.Path, "/server/")
				for _, server := range f.servers {
					if server.ID == id {
						write(plainServer(server))

						return
					}
				}
				w.WriteHeader(404)
			case r.URL.Path == "/organization":
				write(f.organizations)
			case strings.HasPrefix(r.URL.Path, "/user/") && strings.Count(r.URL.Path, "/") == 2:
				write(f.users)
			case strings.HasPrefix(r.URL.Path, "/user/"):
				for _, user := range f.users {
					if r.URL.Path == "/user/"+user["organization"]+"/"+user["id"] {
						write(user)

						return
					}
				}
				w.WriteHeader(404)
			case r.URL.Path == "/admin":
				write(f.admins)
			default:
				w.WriteHeader(404)
			}

			return
		}

		f.posts++
		failure := ""
		if len(f.failures) > 0 {
			failure, f.failures = f.failures[0], f.failures[1:]
		}
		if failure == "502" {
			w.WriteHeader(502)

			return
		}

		body, _ := io.ReadAll(r.Body)
		var reply interface{} = map[string]string{}
		switch {
		case r.URL.Path == "/server":
			server := Server{}
			json.Unmarshal(body, &struct{ Name *string }{&server.Name})
			server.ID = fmt.Sprintf("srv%d", len(f.servers)+1)
			f.servers = append(f.servers, server)
			reply = plainServer(server)
		case r.URL.Path == "/organization":
			organization := Organization{}
			json.Unmarshal(body, &organization)
			organization.ID = fmt.Sprintf("org%d", len(f.organizations)+1)
			f.organizations = append(f.organizations, organization)
			reply = organization
		case strings.HasPrefix(r.URL.Path, "/user/"):
			user := map[string]string{}
			json.Unmarshal(body, &user)
			user["id"] = fmt.Sprintf("usr%d", len(f.users)+1)
			user["organization"] = strings.TrimPrefix(r.URL.Path, "/user/")
			f.users = append(f.users, user)
			reply = []map[string]string{user}
		case r.URL.Path == "/admin":
			admin := Administrator{}
			json.Unmarshal(body, &admin)
			admin["id"] = fmt.Sprintf("adm%d", len(f.admins)+1)
			f.admins = append(f.admins, admin)
			reply = admin
		case strings.HasSuffix(r.URL.Path, "/routes"):
			var routes []Route
			json.Unmarshal(body, &routes)
			f.routes = append(f.routes, routes...)
		case strings.HasSuffix(r.URL.Path, "/route"):
			var route Route
			json.Unmarshal(body, &route)
			f.routes = append(f.routes, route)
		}

		if failure == "drop" {
			dropConnection(w)

			return
		}
		write(reply)
	})
}

func newFakeClient(t *testing.T, instance *fakeInstance) Client {
	t.Helper()
	fastRetry(t)

	server := httptest.NewServer(instance.handler())
	t.Cleanup(server.Close)

	return NewClient(server.URL, "token", "secret", false)
}

func TestAddRoutesToServerRetriesAfterATransientFailure(t *testing.T) {
	instance := &fakeInstance{failures: []string{"502", "502"}}
	apiClient := newFakeClient(t, instance)

	routes := []Route{{Network: "10.1.0.0/24", Comment: "a"}, {Network: "10.2.0.0/24", NetGateway: true}}
	if err := apiClient.AddRoutesToServer("srv1", routes); err != nil {
		t.Fatal(err)
	}

	if instance.posts != 3 {
		t.Fatalf("expected the two failed attempts and the one that went through, got %d", instance.posts)
	}
	if len(instance.routes) != 2 {
		t.Fatalf("expected the two routes once each, got %+v", instance.routes)
	}
}

// The restart swallowed the answer of a request that was served: the routes
// are there, and sending them again is exactly what must not happen.
func TestAddRoutesToServerDoesNotDuplicateAServedRequest(t *testing.T) {
	instance := &fakeInstance{failures: []string{"drop"}}
	apiClient := newFakeClient(t, instance)

	routes := []Route{{Network: "10.1.0.0/24", Comment: "a"}, {Network: "10.2.0.0/24"}}
	if err := apiClient.AddRoutesToServer("srv1", routes); err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 {
		t.Fatalf("a request that was served must not be sent again, got %d POSTs", instance.posts)
	}
	if len(instance.routes) != 2 {
		t.Fatalf("expected the two routes once each, got %+v", instance.routes)
	}
}

func TestAddRouteToServerDoesNotDuplicateAServedRequest(t *testing.T) {
	instance := &fakeInstance{failures: []string{"drop"}}
	apiClient := newFakeClient(t, instance)

	if err := apiClient.AddRouteToServer("srv1", Route{Network: "10.1.0.0/24", Comment: "a"}); err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 || len(instance.routes) != 1 {
		t.Fatalf("expected one POST and one route, got %d POSTs and %+v", instance.posts, instance.routes)
	}
}

func TestAddRouteToServerReportsAnErrorOfTheApi(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(400)
		w.Write([]byte(`{"error": "server_route_invalid", "error_msg": "Route is invalid."}`))
	}))
	defer server.Close()
	fastRetry(t)

	err := NewClient(server.URL, "token", "secret", false).AddRouteToServer("srv1", Route{Network: "nonsense"})
	if err == nil || !strings.Contains(err.Error(), "server_route_invalid") || !strings.Contains(err.Error(), "code=400") {
		t.Fatalf("expected the error of the API with its status, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("an error of the API must not be retried, got %d attempts", attempts)
	}
}

func TestCreateServerAdoptsTheServerAServedRequestCreated(t *testing.T) {
	instance := &fakeInstance{
		servers:  []Server{{ID: "old", Name: "vpn"}},
		failures: []string{"drop"},
	}
	apiClient := newFakeClient(t, instance)

	server, err := apiClient.CreateServer(map[string]interface{}{"name": "vpn"})
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 {
		t.Fatalf("a request that was served must not be sent again, got %d POSTs", instance.posts)
	}
	if server.ID != "srv2" || len(instance.servers) != 2 {
		t.Fatalf("expected the server the lost request created, got %+v among %+v", server, instance.servers)
	}
}

func TestCreateServerRetriesWhenNothingWasCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"502", "502"}}
	apiClient := newFakeClient(t, instance)

	server, err := apiClient.CreateServer(map[string]interface{}{"name": "vpn"})
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 3 || len(instance.servers) != 1 || server.ID != "srv1" {
		t.Fatalf("expected one server after 3 POSTs, got %+v after %d", instance.servers, instance.posts)
	}
}

func TestCreateOrganizationAdoptsTheOrganizationAServedRequestCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"drop"}}
	apiClient := newFakeClient(t, instance)

	organization, err := apiClient.CreateOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 || len(instance.organizations) != 1 || organization.ID != "org1" {
		t.Fatalf("expected the one organization, got %+v after %d POSTs", instance.organizations, instance.posts)
	}
}

func TestCreateOrganizationRetriesWhenNothingWasCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"502"}}
	apiClient := newFakeClient(t, instance)

	organization, err := apiClient.CreateOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 2 || len(instance.organizations) != 1 || organization.ID != "org1" {
		t.Fatalf("expected the one organization after 2 POSTs, got %+v after %d", instance.organizations, instance.posts)
	}
}

// Once the window closes the last failure is the answer, not an endless wait.
func TestAddRoutesToServerGivesUpWhenTheWindowCloses(t *testing.T) {
	instance := &fakeInstance{failures: make([]string, 1000)}
	for i := range instance.failures {
		instance.failures[i] = "502"
	}
	apiClient := newFakeClient(t, instance)
	transportRetryWindow = 200 * time.Millisecond

	err := apiClient.AddRoutesToServer("srv1", []Route{{Network: "10.1.0.0/24"}})
	if err == nil || !strings.Contains(err.Error(), "code=502") {
		t.Fatalf("expected the 502 once the window closed, got %v", err)
	}
	if len(instance.routes) != 0 {
		t.Fatalf("nothing was served, got %+v", instance.routes)
	}
}

// A server of the same name that another resource created while the request
// was failing, on another network, is not the one this request made.
func TestCreateServerDoesNotAdoptALookalike(t *testing.T) {
	fastRetry(t)

	var mu sync.Mutex
	posts := 0
	list := []map[string]interface{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == http.MethodGet {
			if id := strings.TrimPrefix(r.URL.Path, "/server/"); id != r.URL.Path {
				for _, entry := range list {
					if entry["id"] == id {
						json.NewEncoder(w).Encode(entry)
					}
				}

				return
			}
			json.NewEncoder(w).Encode(list)

			return
		}

		posts++
		if posts == 1 {
			list = append(list, map[string]interface{}{"id": "lookalike", "name": "vpn", "network": "10.9.0.0/24", "port": 11000, "protocol": "udp"})
			w.WriteHeader(502)

			return
		}
		created := map[string]interface{}{"id": "mine", "name": "vpn", "network": "10.8.0.0/24", "port": 12000, "protocol": "udp"}
		list = append(list, created)
		json.NewEncoder(w).Encode(created)
	}))
	defer server.Close()

	created, err := NewClient(server.URL, "token", "secret", false).CreateServer(map[string]interface{}{"name": "vpn", "network": "10.8.0.0/24", "port": 12000, "protocol": "udp"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "mine" || posts != 2 {
		t.Fatalf("expected the server of its own after 2 POSTs, got %q after %d", created.ID, posts)
	}
}

func TestCreateServerRefusesToGuessBetweenSeveralNewServers(t *testing.T) {
	fastRetry(t)

	// two servers of the same name and settings appear while the request is
	// in flight: which one it created cannot be told
	var (
		mu    sync.Mutex
		posts int
	)
	list := []map[string]interface{}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(list)

			return
		}
		posts++
		list = append(list,
			map[string]interface{}{"id": "a", "name": "vpn"},
			map[string]interface{}{"id": "b", "name": "vpn"},
		)
		dropConnection(w)
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "token", "secret", false).CreateServer(map[string]interface{}{"name": "vpn"})
	if err == nil || !strings.Contains(err.Error(), "cannot be told") {
		t.Fatalf("expected the ambiguity to be reported, got %v", err)
	}
	if posts != 1 {
		t.Fatalf("nothing may be sent again once it is ambiguous, got %d POSTs", posts)
	}
}

func TestCreateUserAdoptsTheUserAServedRequestCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"drop"}}
	apiClient := newFakeClient(t, instance)

	user, err := apiClient.CreateUser(User{Name: "alice", Organization: "org1"})
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 || len(instance.users) != 1 || user.ID != "usr1" {
		t.Fatalf("expected the one user after 1 POST, got %+v after %d", instance.users, instance.posts)
	}
}

func TestCreateUserRetriesWhenNothingWasCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"502", "502"}}
	apiClient := newFakeClient(t, instance)

	user, err := apiClient.CreateUser(User{Name: "alice", Organization: "org1"})
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 3 || len(instance.users) != 1 || user.ID != "usr1" {
		t.Fatalf("expected the one user after 3 POSTs, got %+v after %d", instance.users, instance.posts)
	}
}

func TestCreateAdministratorAdoptsTheAccountAServedRequestCreated(t *testing.T) {
	instance := &fakeInstance{failures: []string{"drop"}}
	apiClient := newFakeClient(t, instance)

	admin, err := apiClient.CreateAdministrator(Administrator{"username": "ops"})
	if err != nil {
		t.Fatal(err)
	}

	if instance.posts != 1 || len(instance.admins) != 1 || admin.String("id") != "adm1" {
		t.Fatalf("expected the one account after 1 POST, got %+v after %d", instance.admins, instance.posts)
	}
}

// An account that was there before the request is the answer of the API to
// give, not something a failed request made.
func TestCreateAdministratorDoesNotAdoptAnAccountThatWasThereBefore(t *testing.T) {
	instance := &fakeInstance{
		admins:   []Administrator{{"id": "old", "username": "ops"}},
		failures: []string{"502"},
	}
	apiClient := newFakeClient(t, instance)

	if _, err := apiClient.CreateAdministrator(Administrator{"username": "ops"}); err != nil {
		t.Fatal(err)
	}

	if instance.posts != 2 {
		t.Fatalf("expected the request to be sent again, got %d POSTs", instance.posts)
	}
}

func TestActivateSubscriptionRetriesAfterATransientFailure(t *testing.T) {
	instance := &fakeInstance{failures: []string{"502", "drop"}}
	apiClient := newFakeClient(t, instance)

	if err := apiClient.ActivateSubscription("key"); err != nil {
		t.Fatal(err)
	}
	if instance.posts != 3 {
		t.Fatalf("expected the two failed attempts and the one that went through, got %d", instance.posts)
	}
}

// Once the window is closed nothing is sent again, however the pause fell.
func TestRetryCreateDoesNotSendAgainAfterTheWindow(t *testing.T) {
	fastRetry(t)
	transportRetryWindow = 50 * time.Millisecond
	transportRetryFirstWait = 200 * time.Millisecond

	attempts := 0
	start := time.Now()
	result, adopted, err := retryCreate(
		func() attemptResult {
			attempts++

			return attemptResult{status: 502}
		},
		func() (bool, error) { return false, nil },
	)

	if err != nil || adopted || result.status != 502 {
		t.Fatalf("expected the last failure, got %+v adopted=%v err=%v", result, adopted, err)
	}
	if attempts != 1 {
		t.Fatalf("the window closed during the pause, yet the request was sent %d times", attempts)
	}
	if elapsed := time.Since(start); elapsed > 150*time.Millisecond {
		t.Fatalf("the pause outlived the window: %s", elapsed)
	}
}

// A state that cannot be read for a reason waiting does not change is the
// error to report, not the 502 that came before it.
func TestCreateStopsOnANonTransientReconciliationError(t *testing.T) {
	fastRetry(t)

	posts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if posts == 0 {
				json.NewEncoder(w).Encode([]Organization{})

				return
			}
			w.WriteHeader(401)
			w.Write([]byte(`{"error": "unauthorized", "error_msg": "Unauthorized"}`))

			return
		}
		posts++
		w.WriteHeader(502)
	}))
	defer server.Close()

	start := time.Now()
	_, err := NewClient(server.URL, "token", "secret", false).CreateOrganization("acme")

	var apiError *ApiError
	if !errors.As(err, &apiError) || apiError.StatusCode != 401 {
		t.Fatalf("expected the 401 of the reconciliation, got %v", err)
	}
	if posts != 1 {
		t.Fatalf("expected the one request that failed, got %d", posts)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("a refused login must not be retried until the window closes")
	}
}

// Two creates that could be taken for one another run one after the other, so
// the failure of one never adopts the object the other made.
func TestConcurrentCreatesOfOneIdentityDoNotAdoptEachOther(t *testing.T) {
	for round := 0; round < 20; round++ {
		instance := &fakeInstance{failures: []string{"502"}}
		apiClient := newFakeClient(t, instance)

		ids := make([]string, 2)
		var wg sync.WaitGroup
		for i := range ids {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()

				organization, err := apiClient.CreateOrganization("acme")
				if err != nil {
					t.Error(err)

					return
				}
				ids[i] = organization.ID
			}(i)
		}
		wg.Wait()

		if ids[0] == "" || ids[0] == ids[1] || len(instance.organizations) != 2 {
			t.Fatalf("round %d: expected two organizations of their own, got %v among %+v", round, ids, instance.organizations)
		}
	}
}

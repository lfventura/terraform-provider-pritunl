package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// guardClient fakes the slice of the Pritunl API the update path touches and
// tracks which servers are stopped at every instant, so the tests can assert
// the downtime window of concurrent updates and the recovery start of a
// failed one. Methods outside the update path reach the embedded nil
// interface and panic, so an unexpected API call cannot pass silently.
type guardClient struct {
	pritunl.Client

	mu          sync.Mutex
	stopped     map[string]bool
	maxStopped  int
	updateDelay time.Duration
	stopErr     error
	updateErr   error
	// updateErrOnce fails only the first UpdateServer call, so a test can
	// watch the hot path fall back to the guarded window and succeed there.
	updateErrOnce error
	startErr      error
	startSilent   bool
	startCalls    []string
	updateCalls   int
	updated       []*pritunl.Server
}

func newGuardClient() *guardClient {
	return &guardClient{stopped: map[string]bool{}}
}

func (c *guardClient) GetServer(id string) (*pritunl.Server, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	status := pritunl.ServerStatusOnline
	if c.stopped[id] {
		status = pritunl.ServerStatusOffline
	}
	return &pritunl.Server{ID: id, Name: "tfacc-guard", Status: status}, nil
}

// resetDowntimeGuard clears the process-wide downtime state between tests.
func resetDowntimeGuard() {
	serverDowntimeMu.Lock()
	serverLeftDown.client, serverLeftDown.id = nil, ""
	serverDowntimeMu.Unlock()
}

func shortConfirmWindow(t *testing.T) {
	prevTimeout, prevInterval := serverOnlineConfirmTimeout, serverOnlineConfirmInterval
	serverOnlineConfirmTimeout = 50 * time.Millisecond
	serverOnlineConfirmInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		serverOnlineConfirmTimeout = prevTimeout
		serverOnlineConfirmInterval = prevInterval
		// the downtime marker is process-wide: a test that ends blocked must
		// not poison whatever runs after it
		resetDowntimeGuard()
	})
}

func (c *guardClient) StopServer(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopErr != nil {
		// the transport failure lands after the stop took effect
		c.stopped[id] = true
		return c.stopErr
	}
	c.stopped[id] = true
	if len(c.stopped) > c.maxStopped {
		c.maxStopped = len(c.stopped)
	}
	return nil
}

func (c *guardClient) StartServer(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.startCalls = append(c.startCalls, id)
	if c.startErr != nil {
		return c.startErr
	}
	if c.startSilent {
		// the start request is accepted but the server never comes up
		return nil
	}
	delete(c.stopped, id)
	return nil
}

func (c *guardClient) UpdateServer(id string, server *pritunl.Server) error {
	time.Sleep(c.updateDelay)
	c.mu.Lock()
	c.updateCalls++
	c.updated = append(c.updated, server)
	once := c.updateErrOnce
	c.updateErrOnce = nil
	c.mu.Unlock()
	if once != nil {
		return once
	}
	return c.updateErr
}

func (c *guardClient) GetOrganizationsByServer(serverId string) ([]pritunl.Organization, error) {
	return nil, nil
}

func (c *guardClient) GetRoutesByServer(serverId string) ([]pritunl.Route, error) {
	return nil, nil
}

func (c *guardClient) GetHostsByServer(serverId string) ([]pritunl.Host, error) {
	return nil, nil
}

func guardResourceData(t *testing.T, id string) *schema.ResourceData {
	d := schema.TestResourceDataRaw(t, resourceServer().Schema, map[string]interface{}{
		"name": "tfacc-guard",
	})
	d.SetId(id)
	return d
}

// Two concurrent updates never have two servers down at once: the provider
// serializes the stop→start window, whatever parallelism Terraform runs with.
func TestServerUpdateDowntimeIsSerialized(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.updateDelay = 50 * time.Millisecond

	var wg sync.WaitGroup
	for _, id := range []string{"srv1", "srv2"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if diags := resourceUpdateServer(context.Background(), guardResourceData(t, id), pritunl.Client(apiClient)); diags.HasError() {
				t.Errorf("update of %s failed: %v", id, diags)
			}
		}(id)
	}
	wg.Wait()

	if apiClient.maxStopped != 1 {
		t.Fatalf("want at most 1 server down at a time, observed %d", apiClient.maxStopped)
	}
	if len(apiClient.stopped) != 0 {
		t.Fatalf("every server should be back up, still down: %v", apiClient.stopped)
	}
	if len(apiClient.startCalls) != 2 {
		t.Fatalf("want 2 start calls, got %v", apiClient.startCalls)
	}
}

// A failure between the stop and the final start brings the server back up
// before the error returns, and the original error stays first.
func TestServerUpdateFailureStartsServerBack(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.updateErr = errors.New("simulated api rejection")

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if !diags.HasError() {
		t.Fatal("the failed update should return the error")
	}
	if !strings.Contains(diags[0].Summary, "simulated api rejection") {
		t.Fatalf("the original error should come first, got: %s", diags[0].Summary)
	}
	if len(apiClient.startCalls) != 1 || apiClient.startCalls[0] != "srv1" {
		t.Fatalf("the server should be started back after the failure, start calls: %v", apiClient.startCalls)
	}
	if len(apiClient.stopped) != 0 {
		t.Fatalf("no server should be left down, still down: %v", apiClient.stopped)
	}
}

// A stop whose response is lost after Pritunl already stopped the server gets
// the recovery start too, with the stop error first.
func TestServerUpdateStopFailureStartsServerBack(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.stopErr = errors.New("simulated transport failure")

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if !diags.HasError() {
		t.Fatal("the failed stop should return the error")
	}
	if !strings.Contains(diags[0].Summary, "simulated transport failure") {
		t.Fatalf("the stop error should come first, got: %s", diags[0].Summary)
	}
	if len(apiClient.startCalls) != 1 || apiClient.startCalls[0] != "srv1" {
		t.Fatalf("the server should be started back after the failed stop, start calls: %v", apiClient.startCalls)
	}
	if len(apiClient.stopped) != 0 {
		t.Fatalf("no server should be left down, still down: %v", apiClient.stopped)
	}
}

// When the recovery start itself fails, both errors surface and the original
// one stays first.
func TestServerUpdateFailureReportsBothErrors(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.updateErr = errors.New("simulated api rejection")
	apiClient.startErr = errors.New("simulated start failure")

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if len(diags) < 2 {
		t.Fatalf("want the original and the recovery errors, got: %v", diags)
	}
	if !strings.Contains(diags[0].Summary, "simulated api rejection") {
		t.Fatalf("the original error should come first, got: %s", diags[0].Summary)
	}
	if !strings.Contains(diags[len(diags)-1].Summary, "starting the server back") {
		t.Fatalf("the recovery failure should be reported, got: %s", diags[len(diags)-1].Summary)
	}
}

// A server left down by a failed recovery start blocks the queue: the next
// update refuses to stop another server until the down one is confirmed back
// online, and unblocks once it is.
func TestServerUpdateQueueBlocksWhileServerIsLeftDown(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.updateErr = errors.New("simulated api rejection")
	apiClient.startErr = errors.New("simulated start failure")

	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient)); !diags.HasError() {
		t.Fatal("the first update should fail and leave srv1 down")
	}

	apiClient.updateErr = nil
	apiClient.startErr = nil
	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv2"), pritunl.Client(apiClient))
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "still down") {
		t.Fatalf("the queued update should refuse to run while srv1 is down, got: %v", diags)
	}
	if apiClient.stopped["srv2"] {
		t.Fatal("srv2 should not have been stopped while srv1 is down")
	}
	if apiClient.maxStopped != 1 {
		t.Fatalf("want at most 1 server down at a time, observed %d", apiClient.maxStopped)
	}

	// the operator starts srv1 from the console: the queue unblocks
	apiClient.mu.Lock()
	delete(apiClient.stopped, "srv1")
	apiClient.mu.Unlock()
	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv2"), pritunl.Client(apiClient)); diags.HasError() {
		t.Fatalf("the queue should unblock once srv1 is back online: %v", diags)
	}
}

// A start request that is accepted but never brings the server online fails
// the update, marks the server as left down and blocks the queue.
func TestServerUpdateUnconfirmedStartBlocksQueue(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.startSilent = true

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if !diags.HasError() || !strings.Contains(diags[len(diags)-1].Summary, "did not report online") {
		t.Fatalf("the unconfirmed start should fail the update, got: %v", diags)
	}

	apiClient.startSilent = false
	diags = resourceUpdateServer(context.Background(), guardResourceData(t, "srv2"), pritunl.Client(apiClient))
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "still down") {
		t.Fatalf("the queue should stay blocked while srv1 never confirmed online, got: %v", diags)
	}
	if apiClient.stopped["srv2"] {
		t.Fatal("srv2 should not have been stopped while srv1 is down")
	}
}

// A retry on the down server that ends without a confirmed start keeps the
// fence up: only a confirmed-online retry (or an explicit offline) releases
// the queue.
func TestServerUpdateRetryWithoutStartKeepsQueueBlocked(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	apiClient := newGuardClient()
	apiClient.updateErr = errors.New("simulated api rejection")
	apiClient.startErr = errors.New("simulated start failure")

	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient)); !diags.HasError() {
		t.Fatal("the first update should fail and leave srv1 down")
	}

	// the retry reads srv1 as offline, changes an unrelated attribute and
	// therefore never starts the server: it must not release the fence
	apiClient.updateErr = nil
	apiClient.startErr = nil
	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient)); diags.HasError() {
		t.Fatalf("the no-start retry should complete: %v", diags)
	}

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv2"), pritunl.Client(apiClient))
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "still down") {
		t.Fatalf("the queue should stay blocked after a no-start retry, got: %v", diags)
	}
	if apiClient.stopped["srv2"] {
		t.Fatal("srv2 should not have been stopped while srv1 is down")
	}

	apiClient.mu.Lock()
	delete(apiClient.stopped, "srv1")
	apiClient.mu.Unlock()
	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv2"), pritunl.Client(apiClient)); diags.HasError() {
		t.Fatalf("the queue should unblock once srv1 is back online: %v", diags)
	}
}

// The fence binds to the endpoint that left the server down: a second
// provider configuration with a colliding server ID neither bypasses it nor
// clears it against the wrong endpoint.
func TestServerUpdateFenceIsPerEndpoint(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	endpointA := newGuardClient()
	endpointA.updateErr = errors.New("simulated api rejection")
	endpointA.startErr = errors.New("simulated start failure")

	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(endpointA)); !diags.HasError() {
		t.Fatal("the first update should fail and leave srv1 down on endpoint A")
	}

	// same server ID on another endpoint: not a retry, and the liveness check
	// must ask endpoint A, which still reports srv1 down
	endpointB := newGuardClient()
	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(endpointB))
	if !diags.HasError() || !strings.Contains(diags[0].Summary, "still down") {
		t.Fatalf("the colliding ID on another endpoint should stay fenced, got: %v", diags)
	}
	if len(endpointB.stopped) != 0 || len(endpointB.startCalls) != 0 {
		t.Fatal("endpoint B should not have been touched while endpoint A has a server down")
	}

	// endpoint A recovers: the fence lifts for everyone
	endpointA.mu.Lock()
	delete(endpointA.stopped, "srv1")
	endpointA.mu.Unlock()
	if diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(endpointB)); diags.HasError() {
		t.Fatalf("the fence should lift once endpoint A recovers: %v", diags)
	}
}

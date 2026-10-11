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
	startErr    error
	startCalls  []string
}

func newGuardClient() *guardClient {
	return &guardClient{stopped: map[string]bool{}}
}

func (c *guardClient) GetServer(id string) (*pritunl.Server, error) {
	return &pritunl.Server{ID: id, Name: "tfacc-guard", Status: pritunl.ServerStatusOnline}, nil
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
	delete(c.stopped, id)
	return nil
}

func (c *guardClient) UpdateServer(id string, server *pritunl.Server) error {
	time.Sleep(c.updateDelay)
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

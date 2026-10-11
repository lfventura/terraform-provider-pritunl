package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// Lab proofs of the server update downtime guard, driven straight against the
// real Pritunl API: the Terraform acceptance harness can neither observe
// instance statuses mid-apply nor inject an API failure between the real stop
// and the real start, so these tests call the resource update directly with
// the lab client — and, for the failure case, a surgical decorator — while
// watching the instance from outside.

func guardLabPreCheck(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("set TF_ACC to run the lab guard tests")
	}
	preCheck(t)
}

func createLabServer(t *testing.T, name, network string, port int) string {
	t.Helper()

	// A server only starts with an organization attached, so each lab server
	// gets a throwaway one.
	organization, err := testClient.CreateOrganization(name + "-org")
	if err != nil {
		t.Fatalf("creating the lab organization for %s: %s", name, err)
	}
	t.Cleanup(func() {
		_ = testClient.DeleteOrganization(organization.ID)
	})

	server, err := testClient.CreateServer(map[string]interface{}{
		"name":     name,
		"protocol": "udp",
		"cipher":   "aes128",
		"hash":     "sha1",
		"port":     port,
		"network":  network,
	})
	if err != nil {
		t.Fatalf("creating the lab server %s: %s", name, err)
	}
	t.Cleanup(func() {
		_ = testClient.StopServer(server.ID)
		_ = testClient.DeleteServer(server.ID)
	})

	if err := testClient.AttachOrganizationToServer(organization.ID, server.ID); err != nil {
		t.Fatalf("attaching the lab organization to %s: %s", name, err)
	}
	if err := testClient.StartServer(server.ID); err != nil {
		t.Fatalf("starting the lab server %s: %s", name, err)
	}
	waitLabServerStatus(t, server.ID, pritunl.ServerStatusOnline)

	return server.ID
}

func waitLabServerStatus(t *testing.T, id, status string) {
	t.Helper()

	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) {
		server, err := testClient.GetServer(id)
		if err == nil && server.Status == status {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("server %s did not reach status %q in time", id, status)
}

func guardLabResourceData(t *testing.T, id, name string) *schema.ResourceData {
	t.Helper()
	d := schema.TestResourceDataRaw(t, resourceServer().Schema, map[string]interface{}{
		"name": name,
	})
	d.SetId(id)
	return d
}

// The two concurrent updates of running servers never leave both down at
// once: the stop→start window is serialized across the provider process.
func TestAccServerGuardDowntimeIsSerializedLab(t *testing.T) {
	guardLabPreCheck(t)

	firstId := createLabServer(t, "tfacc-guard-serial1", "192.168.231.0/24", 11231)
	secondId := createLabServer(t, "tfacc-guard-serial2", "192.168.232.0/24", 11232)

	stopObserver := make(chan struct{})
	observerDone := make(chan struct{})
	maxOffline := 0
	samples := 0
	go func() {
		defer close(observerDone)
		for {
			select {
			case <-stopObserver:
				return
			case <-time.After(100 * time.Millisecond):
			}
			offline := 0
			for _, id := range []string{firstId, secondId} {
				server, err := testClient.GetServer(id)
				if err != nil || server.Status != pritunl.ServerStatusOnline {
					offline++
				}
			}
			samples++
			if offline > maxOffline {
				maxOffline = offline
			}
		}
	}()

	var wg sync.WaitGroup
	diagErrors := make([]string, 0)
	var diagMu sync.Mutex
	for i, id := range []string{firstId, secondId} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			d := guardLabResourceData(t, id, fmt.Sprintf("tfacc-guard-serial%d-renamed", i+1))
			if diags := resourceUpdateServer(context.Background(), d, testClient); diags.HasError() {
				diagMu.Lock()
				diagErrors = append(diagErrors, fmt.Sprintf("%v", diags))
				diagMu.Unlock()
			}
		}(i, id)
	}
	wg.Wait()
	close(stopObserver)
	<-observerDone

	if len(diagErrors) > 0 {
		t.Fatalf("concurrent updates failed: %s", strings.Join(diagErrors, " | "))
	}
	if maxOffline > 1 {
		t.Fatalf("both servers were observed down at the same time (%d offline across %d samples)", maxOffline, samples)
	}
	waitLabServerStatus(t, firstId, pritunl.ServerStatusOnline)
	waitLabServerStatus(t, secondId, pritunl.ServerStatusOnline)
}

// updateFailingClient passes every call to the lab API except the final
// settings write, which fails as a Pritunl rejection would: the real stop and
// the real recovery start run against the real instance.
type updateFailingClient struct {
	pritunl.Client
}

func (c *updateFailingClient) UpdateServer(id string, server *pritunl.Server) error {
	return errors.New("injected update failure")
}

// A failed update of a running server brings the server back online before
// the error returns.
func TestAccServerGuardFailedUpdateRecoversLab(t *testing.T) {
	guardLabPreCheck(t)

	id := createLabServer(t, "tfacc-guard-recover", "192.168.233.0/24", 11233)

	d := guardLabResourceData(t, id, "tfacc-guard-recover-renamed")
	diags := resourceUpdateServer(context.Background(), d, pritunl.Client(&updateFailingClient{Client: testClient}))

	if !diags.HasError() {
		t.Fatal("the injected failure should surface")
	}
	if !strings.Contains(diags[0].Summary, "injected update failure") {
		t.Fatalf("the original error should come first, got: %s", diags[0].Summary)
	}

	waitLabServerStatus(t, id, pritunl.ServerStatusOnline)

	server, err := testClient.GetServer(id)
	if err != nil {
		t.Fatalf("reading the server back: %s", err)
	}
	if server.Name != "tfacc-guard-recover" {
		t.Fatalf("the failed update should not have renamed the server, got %q", server.Name)
	}
}

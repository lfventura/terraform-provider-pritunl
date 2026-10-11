package provider

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// Lab proofs of the hot update path against the real Pritunl API. The gate is
// the real one, driven by a real diff, so each test also proves which path the
// update takes: a hot-only diff lands through one online request while an
// observer watches the server stay online, and a stopped-only change still
// runs the guarded window and ends online.

// onlineObserver polls the server while an update runs. It records, instead of
// ignoring, every read error and every non-online status, and fails the test
// at the end of the observation.
type onlineObserver struct {
	t       *testing.T
	stop    chan struct{}
	done    sync.WaitGroup
	mu      sync.Mutex
	offline int
	errs    []error
	polls   int
}

func observeOnline(t *testing.T, id string) *onlineObserver {
	o := &onlineObserver{t: t, stop: make(chan struct{})}
	o.done.Add(1)
	go func() {
		defer o.done.Done()
		for {
			select {
			case <-o.stop:
				return
			default:
			}
			server, err := testClient.GetServer(id)
			o.mu.Lock()
			o.polls++
			if err != nil {
				o.errs = append(o.errs, err)
			} else if server.Status != pritunl.ServerStatusOnline {
				o.offline++
			}
			o.mu.Unlock()
			time.Sleep(100 * time.Millisecond)
		}
	}()
	return o
}

// finish keeps observing for a settle period after the update returned (the
// view a client that died right after the PUT would leave behind), then
// fails the test on any offline reading or read error.
func (o *onlineObserver) finish() {
	o.t.Helper()
	time.Sleep(2 * time.Second)
	close(o.stop)
	o.done.Wait()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.polls == 0 {
		o.t.Fatal("the observer never polled the server")
	}
	if len(o.errs) > 0 {
		o.t.Fatalf("the observer failed to read the server %d times, first: %s", len(o.errs), o.errs[0])
	}
	if o.offline > 0 {
		o.t.Fatalf("the server left online during the update: %d offline readings", o.offline)
	}
}

// countingClient forwards to the lab client and counts the calls that tell
// the hot path from the guarded window.
type countingClient struct {
	pritunl.Client
	mu                  sync.Mutex
	stops, starts, puts int
}

func (c *countingClient) StopServer(id string) error {
	c.mu.Lock()
	c.stops++
	c.mu.Unlock()
	return c.Client.StopServer(id)
}

func (c *countingClient) StartServer(id string) error {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return c.Client.StartServer(id)
}

func (c *countingClient) UpdateServer(id string, server *pritunl.Server) error {
	c.mu.Lock()
	c.puts++
	c.mu.Unlock()
	return c.Client.UpdateServer(id, server)
}

// labUpdate runs one real update and returns the calls it made.
func labUpdate(t *testing.T, id string, stateOverrides, overrides map[string]interface{}) *countingClient {
	t.Helper()
	// The update copies name, port and network from the config unconditionally,
	// so the settled state must be the real server's, or the PUT would carry
	// the helper's placeholders and touch stopped-only fields.
	live, err := testClient.GetServer(id)
	if err != nil {
		t.Fatalf("reading the live server: %s", err)
	}
	settled := map[string]interface{}{"name": live.Name, "port": live.Port, "network": live.Network}
	for k, v := range stateOverrides {
		settled[k] = v
	}
	d := gateResourceDataWithState(t, settled, overrides)
	d.SetId(id)
	counter := &countingClient{Client: testClient}
	if diags := resourceUpdateServer(context.Background(), d, pritunl.Client(counter)); diags.HasError() {
		t.Fatalf("update failed: %+v", diags)
	}
	return counter
}

// requireHotCalls asserts the update was exactly one PUT with no stop or start.
func requireHotCalls(t *testing.T, c *countingClient) {
	t.Helper()
	if c.stops != 0 || c.starts != 0 || c.puts != 1 {
		t.Fatalf("a hot update must be one PUT and nothing else: stops=%d starts=%d puts=%d", c.stops, c.starts, c.puts)
	}
}

func labServerMustBeOnline(t *testing.T, id string) *pritunl.Server {
	t.Helper()
	server, err := testClient.GetServer(id)
	if err != nil {
		t.Fatalf("reading the server back: %s", err)
	}
	if server.Status != pritunl.ServerStatusOnline {
		t.Fatalf("the server is not online after the update: %s", server.Status)
	}
	return server
}

func TestAccServerHotRenameStaysOnlineLab(t *testing.T) {
	guardLabPreCheck(t)

	id := createLabServer(t, "tfacc-hot-a", "192.168.237.0/24", 11237)
	renamed := "tfacc-hot-a-renamed"

	observer := observeOnline(t, id)
	requireHotCalls(t, labUpdate(t, id, nil, map[string]interface{}{"name": renamed}))
	observer.finish()

	if server := labServerMustBeOnline(t, id); server.Name != renamed {
		t.Fatalf("the rename did not land: name=%s", server.Name)
	}
}

// pre_connect_msg is set and then cleared on a running server: both updates
// are hot, the server stays online, and Pritunl accepts the empty value.
func TestAccServerHotPreConnectMsgStaysOnlineLab(t *testing.T) {
	guardLabPreCheck(t)

	id := createLabServer(t, "tfacc-hot-b", "192.168.238.0/24", 11238)

	observer := observeOnline(t, id)
	requireHotCalls(t, labUpdate(t, id, nil, map[string]interface{}{"pre_connect_msg": "hello from the lab"}))
	if server := labServerMustBeOnline(t, id); server.PreConnectMsg != "hello from the lab" {
		t.Fatalf("the message did not land: %q", server.PreConnectMsg)
	}
	requireHotCalls(t, labUpdate(t, id, map[string]interface{}{"pre_connect_msg": "hello from the lab"}, map[string]interface{}{"pre_connect_msg": ""}))
	observer.finish()

	if server := labServerMustBeOnline(t, id); server.PreConnectMsg != "" {
		t.Fatalf("the message was not cleared online: %q", server.PreConnectMsg)
	}
}

func TestAccServerHotReplicaCountStaysOnlineLab(t *testing.T) {
	guardLabPreCheck(t)

	id := createLabServer(t, "tfacc-hot-c", "192.168.239.0/24", 11239)

	observer := observeOnline(t, id)
	requireHotCalls(t, labUpdate(t, id, nil, map[string]interface{}{"replica_count": 2}))
	observer.finish()

	if server := labServerMustBeOnline(t, id); server.ReplicaCount != 2 {
		t.Fatalf("the replica count did not land: %d", server.ReplicaCount)
	}
}

// A stopped-only change (the port) is not hot: the update runs the guarded
// stop→start window, the new port lands and the server ends online.
func TestAccServerStoppedOnlyChangeTakesGuardedPathLab(t *testing.T) {
	guardLabPreCheck(t)

	id := createLabServer(t, "tfacc-hot-d", "192.168.241.0/24", 11241)

	d := gateResourceData(t, map[string]interface{}{"port": 11242})
	if serverUpdateIsHotOnly(d) {
		t.Fatal("a port change must not take the hot path")
	}

	counter := labUpdate(t, id, nil, map[string]interface{}{"port": 11242})
	if counter.stops != 1 || counter.starts != 1 || counter.puts != 1 {
		t.Fatalf("a stopped-only change must run the guarded window once: stops=%d starts=%d puts=%d", counter.stops, counter.starts, counter.puts)
	}
	waitLabServerStatus(t, id, pritunl.ServerStatusOnline)
	if server := labServerMustBeOnline(t, id); server.Port != 11242 {
		t.Fatalf("the port did not land: %d", server.Port)
	}
}

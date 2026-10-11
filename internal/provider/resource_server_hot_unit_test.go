package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// forceHotGate pins the hot-only decision for a test, restoring the real gate
// afterwards, so a flow test drives the update down a chosen path; the gate's
// own decisions are covered by TestServerHotGateDecision on real diffs.
func forceHotGate(t *testing.T, hot bool) {
	prev := serverUpdateIsHotOnly
	serverUpdateIsHotOnly = func(*schema.ResourceData) bool { return hot }
	t.Cleanup(func() { serverUpdateIsHotOnly = prev })
}

// A hot-only update goes straight to the PUT: the server is never stopped,
// never started, and the downtime window never opens.
func TestServerHotUpdateSkipsDowntimeWindow(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	forceHotGate(t, true)
	apiClient := newGuardClient()

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if diags.HasError() {
		t.Fatalf("hot update failed: %+v", diags)
	}
	if apiClient.maxStopped != 0 {
		t.Fatalf("hot update stopped a server: maxStopped=%d", apiClient.maxStopped)
	}
	if len(apiClient.startCalls) != 0 {
		t.Fatalf("hot update started a server: %v", apiClient.startCalls)
	}
}

// A hot-only PUT answered with server_not_offline is converging live drift on
// a stopped-only field: the update falls back to the guarded stop→start
// window and completes there.
func TestServerHotUpdateFallsBackOnNotOffline(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	forceHotGate(t, true)
	apiClient := newGuardClient()
	apiClient.updateErrOnce = errors.New("Non-200 response on updating the server\nbody={\"error\": \"server_not_offline\", \"error_msg\": \"Server must be offline to modify settings.\"}")

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if diags.HasError() {
		t.Fatalf("fallback update failed: %+v", diags)
	}
	if apiClient.maxStopped != 1 {
		t.Fatalf("fallback should run the guarded window once: maxStopped=%d", apiClient.maxStopped)
	}
	if len(apiClient.startCalls) != 1 || apiClient.startCalls[0] != "srv1" {
		t.Fatalf("fallback should start the server back: %v", apiClient.startCalls)
	}
	if apiClient.updateCalls != 2 {
		t.Fatalf("fallback should PUT twice (hot attempt, then the guarded window): got %d", apiClient.updateCalls)
	}
	if len(apiClient.stopped) != 0 {
		t.Fatalf("fallback ended with servers stopped: %v", apiClient.stopped)
	}
	if status := guardServerStatus(t, apiClient, "srv1"); status != pritunl.ServerStatusOnline {
		t.Fatalf("fallback ended with the server %s, want online", status)
	}
	if serverLeftDown.id != "" {
		t.Fatalf("fallback left the fence up for %s", serverLeftDown.id)
	}
}

// Any other failure of the hot PUT surfaces as-is: the guarded window is not
// a retry mechanism for errors that have nothing to do with the server state.
func TestServerHotUpdateSurfacesOtherErrors(t *testing.T) {
	resetDowntimeGuard()
	shortConfirmWindow(t)
	forceHotGate(t, true)
	apiClient := newGuardClient()
	apiClient.updateErrOnce = errors.New("Non-200 response on updating the server\nbody={\"error\": \"network_invalid\"}")

	diags := resourceUpdateServer(context.Background(), guardResourceData(t, "srv1"), pritunl.Client(apiClient))
	if !diags.HasError() {
		t.Fatal("hot update should surface the PUT failure")
	}
	if !strings.Contains(fmt.Sprint(diags), "network_invalid") {
		t.Fatalf("the diagnostic lost the original error text: %+v", diags)
	}
	if apiClient.updateCalls != 1 {
		t.Fatalf("a non-fallback error must not retry: %d PUTs", apiClient.updateCalls)
	}
	if apiClient.maxStopped != 0 || len(apiClient.startCalls) != 0 {
		t.Fatalf("failed hot update must not touch the server state: maxStopped=%d starts=%v", apiClient.maxStopped, apiClient.startCalls)
	}
}

func guardServerStatus(t *testing.T, c *guardClient, id string) string {
	t.Helper()
	server, err := c.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	return server.Status
}

// gateResourceData builds a ResourceData carrying a real diff between a
// settled state and a config that differs from it only by the overrides.
func gateResourceData(t *testing.T, overrides map[string]interface{}) *schema.ResourceData {
	t.Helper()
	return gateResourceDataWithState(t, nil, overrides)
}

// gateResourceDataWithState also lets the settled state differ from the base.
func gateResourceDataWithState(t *testing.T, stateOverrides, overrides map[string]interface{}) *schema.ResourceData {
	t.Helper()
	r := resourceServer()
	base := map[string]interface{}{
		"name":     "tfacc-gate",
		"protocol": "udp",
		"cipher":   "aes128",
		"hash":     "sha1",
		"port":     11300,
		"network":  "192.168.240.0/24",
	}
	settledConfig := map[string]interface{}{}
	for k, v := range base {
		settledConfig[k] = v
	}
	for k, v := range stateOverrides {
		settledConfig[k] = v
	}
	settled := schema.TestResourceDataRaw(t, r.Schema, settledConfig)
	settled.SetId("srv-gate")
	state := settled.State()

	config := map[string]interface{}{}
	for k, v := range base {
		config[k] = v
	}
	for k, v := range stateOverrides {
		config[k] = v
	}
	for k, v := range overrides {
		config[k] = v
	}
	diff, err := r.Diff(context.Background(), state, terraform.NewResourceConfigRaw(config), nil)
	if err != nil {
		t.Fatalf("building the diff: %s", err)
	}
	d, err := schema.InternalMap(r.Schema).Data(state, diff)
	if err != nil {
		t.Fatalf("building the resource data: %s", err)
	}
	return d
}

// The real gate, driven by real diffs: only a diff confined to the hot
// arguments takes the hot path.
func TestServerHotGateDecision(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]interface{}
		hot       bool
	}{
		{"empty diff", nil, false},
		{"name only", map[string]interface{}{"name": "tfacc-gate-renamed"}, true},
		{"pre_connect_msg only", map[string]interface{}{"pre_connect_msg": "welcome"}, true},
		{"replica_count only", map[string]interface{}{"replica_count": 2}, true},
		{"all hot arguments", map[string]interface{}{"name": "tfacc-gate-renamed", "pre_connect_msg": "welcome", "replica_count": 2}, true},
		{"name and port", map[string]interface{}{"name": "tfacc-gate-renamed", "port": 11301}, false},
		{"port only", map[string]interface{}{"port": 11301}, false},
		{"pre_connect_msg and debug", map[string]interface{}{"pre_connect_msg": "welcome", "debug": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := serverUpdateIsHotOnly(gateResourceData(t, tc.overrides)); got != tc.hot {
				t.Fatalf("serverUpdateIsHotOnly = %v, want %v", got, tc.hot)
			}
		})
	}
}

// An emptied pre_connect_msg reaches the API as an explicit "", on both the
// hot and the guarded path, while an untouched one stays out of the document.
func TestServerPreConnectMsgClearReachesAPI(t *testing.T) {
	marshal := func(s *pritunl.Server) map[string]interface{} {
		raw, err := s.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]interface{}{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if v, ok := marshal(&pritunl.Server{Name: "a"})["pre_connect_msg"]; ok {
		t.Fatalf("an untouched message must stay out of the document, got %v", v)
	}
	if v := marshal(&pritunl.Server{Name: "a", PreConnectMsg: "hi"})["pre_connect_msg"]; v != "hi" {
		t.Fatalf("a set message must marshal as before, got %v", v)
	}
	if v, ok := marshal(&pritunl.Server{Name: "a", ClearPreConnectMsg: true})["pre_connect_msg"]; !ok || v != "" {
		t.Fatalf("an explicit clear must marshal as \"\", got %v (present=%v)", v, ok)
	}

	for _, hot := range []bool{true, false} {
		resetDowntimeGuard()
		shortConfirmWindow(t)
		forceHotGate(t, hot)
		apiClient := newGuardClient()
		diags := resourceUpdateServer(context.Background(), clearMsgResourceData(t), pritunl.Client(apiClient))
		if diags.HasError() {
			t.Fatalf("hot=%v: %+v", hot, diags)
		}
		last := apiClient.updated[len(apiClient.updated)-1]
		if !last.ClearPreConnectMsg || last.PreConnectMsg != "" {
			t.Fatalf("hot=%v: the update did not request an explicit clear: %+v", hot, last)
		}
		if v, ok := marshal(last)["pre_connect_msg"]; !ok || v != "" {
			t.Fatalf("hot=%v: the PUT body lacks the explicit empty message", hot)
		}
	}
}

// clearMsgResourceData is a resource whose state carries a message and whose
// config empties it.
func clearMsgResourceData(t *testing.T) *schema.ResourceData {
	t.Helper()
	d := gateResourceDataWithState(t, map[string]interface{}{"pre_connect_msg": "welcome"}, map[string]interface{}{"pre_connect_msg": ""})
	return d
}

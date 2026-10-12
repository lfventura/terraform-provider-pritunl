package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

func TestRouteCommentStoredFilteredPlansNoChange(t *testing.T) {
	state := &terraform.InstanceState{
		ID: "6a207ec38d6b318a3f2e3512",
		Attributes: map[string]string{
			"id":                  "6a207ec38d6b318a3f2e3512",
			"name":                "YAPEPERU-GateData-stg",
			"protocol":            "udp",
			"route.#":             "2",
			"route.0.network":     "0.0.0.0/0",
			"route.0.comment":     "Routed-alltrafficthroughfirewallnoNAT",
			"route.0.nat":         "false",
			"route.0.net_gateway": "false",
			"route.1.network":     "31.186.239.0/24",
			"route.1.comment":     "Netskope NewEdge - bypass VPN (keep local POP, avoid US hairpin)",
			"route.1.nat":         "false",
			"route.1.net_gateway": "true",
		},
	}

	config := func(firstComment string) *terraform.ResourceConfig {
		return terraform.NewResourceConfigRaw(map[string]interface{}{
			"name": "YAPEPERU-GateData-stg",
			"route": []interface{}{
				map[string]interface{}{"network": "0.0.0.0/0", "comment": firstComment, "nat": false, "net_gateway": false},
				map[string]interface{}{"network": "31.186.239.0/24", "comment": "Netskope NewEdge - bypass VPN (keep local POP, avoid US hairpin)", "nat": false, "net_gateway": true},
			},
		})
	}

	diff, err := resourceServer().Diff(context.Background(), state, config("Routed - all traffic through firewall, no NAT"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff != nil {
		if attr, ok := diff.Attributes["route.0.comment"]; ok {
			t.Fatalf("a comment stored filtered still plans a change: %#v", attr)
		}
	}

	diff, err = resourceServer().Diff(context.Background(), state, config("Default route"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || diff.Attributes["route.0.comment"] == nil {
		t.Fatal("a really different comment plans no change")
	}
}

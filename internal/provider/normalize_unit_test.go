package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func TestPritunlFilterStr(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"empty stays empty":              {"", ""},
		"spaces and commas are dropped":  {"Routed - all traffic through firewall, no NAT", "Routed-alltrafficthroughfirewallnoNAT"},
		"parentheses are dropped":        {"Netskope NewEdge - bypass VPN (keep local POP, avoid US hairpin)", "NetskopeNewEdge-bypassVPNkeeplocalPOPavoidUShairpin"},
		"safe characters are kept":       {"a-b=c_d@e.f:g/h!i#j$k&l*m+n?o^p|q~r", "a-b=c_d@e.f:g/h!i#j$k&l*m+n?o^p|q~r"},
		"letters beyond ascii are kept":  {"Conexão São Paulo", "ConexãoSãoPaulo"},
		"numbers beyond digits are kept": {"Rack ² floor ½ Ⅻ", "Rack²floor½Ⅻ"},
		"already filtered is unchanged":  {"Routed-alltrafficthroughfirewallnoNAT", "Routed-alltrafficthroughfirewallnoNAT"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := pritunlFilterStr(c.in); got != c.want {
				t.Fatalf("pritunlFilterStr(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSuppressRouteCommentDiff(t *testing.T) {
	cases := map[string]struct {
		stored, configured string
		suppressed         bool
	}{
		"filtered by a recent release": {"Routed-alltrafficthroughfirewallnoNAT", "Routed - all traffic through firewall, no NAT", true},
		"stored as written":            {"Routed - all traffic through firewall, no NAT", "Routed - all traffic through firewall, no NAT", true},
		"a different comment":          {"Routed-alltrafficthroughfirewallnoNAT", "Netskope NewEdge", false},
		"a comment added":              {"", "Netskope NewEdge", false},
		"a comment removed":            {"Netskope NewEdge", "", false},
		"a superscript changed":        {"Rack²", "Rack ³", false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := suppressRouteCommentDiff("route.0.comment", c.stored, c.configured, nil); got != c.suppressed {
				t.Fatalf("suppressRouteCommentDiff(%q, %q) = %v, want %v", c.stored, c.configured, got, c.suppressed)
			}
		})
	}
}

func TestSSOURLKey(t *testing.T) {
	cases := map[string]struct {
		in   string
		want string
	}{
		"empty stays empty":             {"", ""},
		"bare host":                     {"gate.stg.yape.tech", "gate.stg.yape.tech"},
		"https scheme is dropped":       {"https://gate.stg.yape.tech", "gate.stg.yape.tech"},
		"http scheme is dropped":        {"http://gate.stg.yape.tech", "gate.stg.yape.tech"},
		"host is lowercased":            {"HTTPS://Gate.STG.yape.tech", "gate.stg.yape.tech"},
		"a lone slash is no path":       {"https://gate.stg.yape.tech/", "gate.stg.yape.tech"},
		"port is kept":                  {"https://vpn.example.com:8443", "vpn.example.com:8443"},
		"port leading zeros go":         {"vpn.example.com:0443", "vpn.example.com:443"},
		"surrounding space is gone":     {"  vpn.example.com \n", "vpn.example.com"},
		"bare ipv6 is bracketed":        {"2001:db8::1", "[2001:db8::1]"},
		"ipv4 mapped ipv6 is bracketed": {"::ffff:1.2.3.4", "[::ffff:1.2.3.4]"},
		"ipv6 url keeps its port":       {"https://[2001:db8::1]:8443", "[2001:db8::1]:8443"},
		"a path is part of the key":     {"https://vpn.example.com/sso", "https://vpn.example.com/sso?#"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := ssoURLKey(c.in); got != c.want {
				t.Fatalf("ssoURLKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestSuppressServerSSOURLDiff(t *testing.T) {
	cases := map[string]struct {
		stored, configured string
		suppressed         bool
	}{
		"older release stores https": {"https://gate.qa.yape.tech", "gate.qa.yape.tech", true},
		"recent release stores bare": {"gate.qa.yape.tech", "https://gate.qa.yape.tech", true},
		"same value":                 {"gate.qa.yape.tech", "gate.qa.yape.tech", true},
		"another host":               {"https://gate.qa.yape.tech", "gate.stg.yape.tech", false},
		"another port":               {"vpn.example.com", "vpn.example.com:8443", false},
		"first configuration":        {"", "gate.qa.yape.tech", false},
		"a path changed":             {"https://vpn.example.com/a", "https://vpn.example.com/b", false},
		"a path added":               {"https://vpn.example.com", "https://vpn.example.com/sso", false},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := suppressServerSSOURLDiff("server_sso_url", c.stored, c.configured, nil); got != c.suppressed {
				t.Fatalf("suppressServerSSOURLDiff(%q, %q) = %v, want %v", c.stored, c.configured, got, c.suppressed)
			}
		})
	}
}

func TestNormalizationIsWiredIntoTheSchemas(t *testing.T) {
	settings := resourceSettings().Schema["server_sso_url"]
	if settings.DiffSuppressFunc == nil {
		t.Fatal("pritunl_settings.server_sso_url has no DiffSuppressFunc")
	}

	route := resourceServer().Schema["route"].Elem.(*schema.Resource)
	if route.Schema["comment"].DiffSuppressFunc == nil {
		t.Fatal("pritunl_server.route.comment has no DiffSuppressFunc")
	}
}

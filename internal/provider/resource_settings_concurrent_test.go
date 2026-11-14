package provider

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

const concurrentServerCount = 6

// Pritunl restarts its web server when the certificate changes, and the
// resources Terraform applies in parallel with the settings meet that restart
// halfway through their own calls. The settings carry a fresh certificate and
// several servers with routes are created next to them, the way one apply of a
// whole instance does it: every one of them has to come out created, none
// tainted by a call that landed in the restart.
//
// The restart lasts a fraction of a second, so next to the servers a steady
// stream of route additions goes through the client the tests use, which makes
// sure a call lands in it whatever the pace of the apply: none of them may
// fail either.
//
// Like TestAccPritunlSettings it pushes a certificate to the instance the whole
// suite runs against, and destroying the configuration resets it.
func TestAccPritunlSettingsConcurrentWithServers(t *testing.T) {
	if os.Getenv("PRITUNL_SKIP_SETTINGS_ACC_TEST") != "" {
		t.Skip("PRITUNL_SKIP_SETTINGS_ACC_TEST is set, skipping the pritunl_settings acceptance test")
	}

	cert, key := generateSelfSignedCertificate(t)

	checks := []resource.TestCheckFunc{
		testAccCheckSettingsCertificate(cert),
	}
	for i := 0; i < concurrentServerCount; i++ {
		name := fmt.Sprintf("pritunl_server.test_%d", i)
		checks = append(checks,
			resource.TestCheckResourceAttr(name, "route.#", "3"),
			testAccCheckServerRoutesPresent(name, 3),
		)
	}

	traffic := &routeTraffic{t: t}
	t.Cleanup(traffic.halt)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { preCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testAccCheckSettingsCertificateReset(cert),
		Steps: []resource.TestStep{
			{
				PreConfig: traffic.start,
				Config:    testPritunlSettingsConcurrentConfig(cert, key),
				Check:     resource.ComposeTestCheckFunc(checks...),
			},
		},
	})

	traffic.halt()
	traffic.verify()
}

// routeTraffic adds routes to a server of its own, one after the other, for as
// long as it runs, and keeps the failures.
type routeTraffic struct {
	t *testing.T

	serverId string
	done     chan struct{}
	wg       sync.WaitGroup

	mu       sync.Mutex
	requests int
	failures []error
}

func (r *routeTraffic) start() {
	if r.done != nil {
		return
	}

	server, err := testClient.CreateServer(map[string]interface{}{
		"name":     "tfacc-concurrent-traffic",
		"network":  "10.82.0.0/24",
		"port":     15199,
		"protocol": "udp",
		"cipher":   "aes128",
		"hash":     "sha1",
	})
	if err != nil {
		r.t.Fatalf("failed to create the server the route traffic goes to: %s", err)
	}
	r.serverId = server.ID
	r.done = make(chan struct{})

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()

		for i := 0; ; i++ {
			select {
			case <-r.done:
				return
			case <-time.After(40 * time.Millisecond):
			}

			route := pritunl.Route{Network: fmt.Sprintf("10.83.%d.%d/32", i/250, i%250), Comment: "tfacc-traffic"}
			err := testClient.AddRouteToServer(r.serverId, route)

			r.mu.Lock()
			r.requests++
			if err != nil {
				r.failures = append(r.failures, err)
			}
			r.mu.Unlock()
		}
	}()
}

// halt stops the traffic and deletes the server it went to; calling it again
// does nothing.
func (r *routeTraffic) halt() {
	if r.done == nil {
		return
	}

	select {
	case <-r.done:
		return
	default:
	}

	close(r.done)
	r.wg.Wait()

	if err := testClient.DeleteServer(r.serverId); err != nil {
		r.t.Errorf("failed to delete the server the route traffic went to: %s", err)
	}
}

func (r *routeTraffic) verify() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.t.Logf("%d routes were added while the settings were applied", r.requests)
	if len(r.failures) > 0 {
		r.t.Fatalf("%d of %d route additions failed, the first: %s", len(r.failures), r.requests, r.failures[0])
	}
}

// The routes the instance holds for the server, read back through the api, are
// the declared ones and each of them exactly once.
func testAccCheckServerRoutesPresent(resourceName string, declared int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		server := s.RootModule().Resources[resourceName]
		if server == nil {
			return fmt.Errorf("%s is not in the state", resourceName)
		}

		routes, err := testClient.GetRoutesByServer(server.Primary.ID)
		if err != nil {
			return err
		}

		seen := map[string]int{}
		for _, route := range routes {
			if route.VirtualNetwork {
				continue
			}
			seen[route.Network]++
		}

		if len(seen) != declared {
			return fmt.Errorf("%s holds %d routes besides its own network, want %d: %v", resourceName, len(seen), declared, seen)
		}
		for network, count := range seen {
			if count != 1 {
				return fmt.Errorf("%s holds the route %s %d times", resourceName, network, count)
			}
		}

		return nil
	}
}

func testPritunlSettingsConcurrentConfig(cert, key string) string {
	var config strings.Builder

	fmt.Fprintf(&config, `
		resource "pritunl_settings" "test" {
			server_cert = %[1]q
			server_key  = %[2]q
		}
	`, cert, key)

	for i := 0; i < concurrentServerCount; i++ {
		fmt.Fprintf(&config, `
		resource "pritunl_server" "test_%[1]d" {
			name     = "tfacc-concurrent-%[1]d"
			network  = "10.78.%[1]d.0/24"
			port     = %[2]d
			protocol = "udp"

			route {
				network = "10.79.%[1]d.0/24"
				comment = "tfacc-route"
			}

			route {
				network = "10.80.%[1]d.0/24"
				comment = "tfacc-route"
			}

			route {
				network = "10.81.%[1]d.0/24"
				comment = "tfacc-route"
				net_gateway = true
			}
		}
		`, i, 15100+i)
	}

	return config.String()
}

package provider

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// A server declared online is running when the apply ends, on create and
// after an update that restarts it.
func TestAccPritunlServerStatusOnline(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { preCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testPritunlServerDestroy,
		Steps: []resource.TestStep{
			{
				Config: testPritunlServerOnlineConfig("", "tfacc-start-online", 210, 15210),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("pritunl_server.test", "status", pritunl.ServerStatusOnline),
					testAccCheckServerLiveStatus("pritunl_server.test", pritunl.ServerStatusOnline),
				),
			},
			{
				// a new port restarts the server
				Config: testPritunlServerOnlineConfig("", "tfacc-start-online", 210, 15211),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("pritunl_server.test", "port", "15211"),
					resource.TestCheckResourceAttr("pritunl_server.test", "status", pritunl.ServerStatusOnline),
					testAccCheckServerLiveStatus("pritunl_server.test", pritunl.ServerStatusOnline),
				),
			},
		},
	})
}

// The production failure, reproduced against a real Pritunl: the answer of a
// start is lost while Pritunl is still running it, and the start then times
// out ("Server start timed out", then "Startup doc lost, stopping server" in
// the journal), which puts the server back offline after the provider took it
// as online.
//
// Two faults are injected, because neither happens on demand:
//
//   - the instance of the server comes up slower than Pritunl waits for it
//     (4 seconds plus vpn.op_timeout, 25 seconds): a wrapper around sysctl in
//     the container, which the instance runs before it reports that it
//     started, sleeps 40 seconds once;
//   - the provider talks to Pritunl through a proxy that forwards the first
//     start request of the server and answers it with a 502 after two
//     seconds, while Pritunl goes on running it, the way a web server that
//     restarts under the request answers.
//
// The server has to be online once the start Pritunl gave up has been
// reverted. It needs the docker container of the instance, named by
// PRITUNL_ACC_CONTAINER, to install the wrapper.
func TestAccPritunlServerStartLostAndTimedOut(t *testing.T) {
	container := os.Getenv("PRITUNL_ACC_CONTAINER")
	if container == "" {
		t.Skip("PRITUNL_ACC_CONTAINER is not set: the test needs the docker container of the instance")
	}

	slowStart := installSlowStart(t, container, 40*time.Second)
	proxy := newStartCuttingProxy(t, os.Getenv("PRITUNL_URL"), 2*time.Second)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { preCheck(t) },
		ProviderFactories: providerFactories,
		CheckDestroy:      testPritunlServerDestroy,
		Steps: []resource.TestStep{
			{
				PreConfig: slowStart,
				Config:    testPritunlServerOnlineConfig(proxy.URL, "tfacc-start-lost", 220, 15220),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("pritunl_server.test", "status", pritunl.ServerStatusOnline),
					testAccCheckServerStaysOnline("pritunl_server.test", 45*time.Second),
					proxy.checkCut,
				),
			},
		},
	})
}

// testAccCheckServerLiveStatus reads the server through the API.
func testAccCheckServerLiveStatus(resourceName, want string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		server, err := testServerFromState(s, resourceName)
		if err != nil {
			return err
		}
		if server.Status != want {
			return fmt.Errorf("%s reads %q through the API, want %q", resourceName, server.Status, want)
		}
		return nil
	}
}

// testAccCheckServerStaysOnline reads the server for a while after the apply:
// a start Pritunl gives up puts the server offline up to about 30 seconds
// after it began, so a server that only looked online when the apply ended
// shows here.
func testAccCheckServerStaysOnline(resourceName string, period time.Duration) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		deadline := time.Now().Add(period)
		for {
			server, err := testServerFromState(s, resourceName)
			if err != nil {
				return err
			}
			if server.Status != pritunl.ServerStatusOnline {
				return fmt.Errorf("%s went %s after the apply reported it online", resourceName, server.Status)
			}
			if !time.Now().Before(deadline) {
				return nil
			}
			time.Sleep(2 * time.Second)
		}
	}
}

func testServerFromState(s *terraform.State, resourceName string) (*pritunl.Server, error) {
	state := s.RootModule().Resources[resourceName]
	if state == nil {
		return nil, fmt.Errorf("%s is not in the state", resourceName)
	}
	return testClient.GetServer(state.Primary.ID)
}

// installSlowStart wraps sysctl in the container so that, once a flag file
// exists, its next run sleeps for delay and removes the flag. The returned
// function arms the flag; the wrapper is removed when the test ends.
func installSlowStart(t *testing.T, container string, delay time.Duration) func() {
	t.Helper()

	script := fmt.Sprintf(`set -e
S=$(command -v sysctl)
if [ ! -e "$S.tfacc-real" ]; then mv "$S" "$S.tfacc-real"; fi
cat > "$S" <<EOF
#!/bin/sh
if [ -e /tmp/tfacc-slow-start ]; then rm -f /tmp/tfacc-slow-start; sleep %d; fi
exec $S.tfacc-real "\$@"
EOF
chmod +x "$S"`, int(delay.Seconds()))
	dockerExec(t, container, script)

	t.Cleanup(func() {
		dockerExec(t, container, `S=$(command -v sysctl); rm -f /tmp/tfacc-slow-start; if [ -e "$S.tfacc-real" ]; then mv -f "$S.tfacc-real" "$S"; fi`)
	})

	return func() {
		dockerExec(t, container, "touch /tmp/tfacc-slow-start")
	}
}

func dockerExec(t *testing.T, container, script string) {
	t.Helper()
	out, err := exec.Command("docker", "exec", container, "sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("docker exec in %s failed: %s: %s", container, err, out)
	}
}

// startCuttingProxy passes every request to Pritunl, except the first start
// of each server: that one is forwarded on its own, so Pritunl runs it to the
// end, and answered with a 502 after a delay, as a restarting web server
// answers a request it was serving.
type startCuttingProxy struct {
	*httptest.Server

	mu  sync.Mutex
	cut map[string]bool
}

func newStartCuttingProxy(t *testing.T, target string, delay time.Duration) *startCuttingProxy {
	t.Helper()

	upstream, err := url.Parse(target)
	if err != nil {
		t.Fatalf("PRITUNL_URL: %s", err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}

	reverse := httputil.NewSingleHostReverseProxy(upstream)
	reverse.Transport = transport

	p := &startCuttingProxy{cut: map[string]bool{}}
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/operation/start") {
			p.mu.Lock()
			first := !p.cut[r.URL.Path]
			p.cut[r.URL.Path] = true
			p.mu.Unlock()

			if first {
				forward, err := http.NewRequestWithContext(context.Background(), r.Method, strings.TrimSuffix(target, "/")+r.URL.Path, nil)
				if err != nil {
					t.Errorf("proxy: %s", err)
					return
				}
				forward.Header = r.Header.Clone()
				go func() {
					resp, err := transport.RoundTrip(forward)
					if err == nil {
						io.Copy(io.Discard, resp.Body)
						resp.Body.Close()
						t.Logf("proxy: the start cut short got %d from Pritunl", resp.StatusCode)
					}
				}()

				time.Sleep(delay)
				w.WriteHeader(http.StatusBadGateway)
				return
			}
		}
		reverse.ServeHTTP(w, r)
	}))
	t.Cleanup(p.Close)

	return p
}

// checkCut makes sure the fault was injected.
func (p *startCuttingProxy) checkCut(*terraform.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.cut) == 0 {
		return fmt.Errorf("no start request went through the proxy")
	}
	return nil
}

// testPritunlServerOnlineConfig declares an organization and a server online;
// providerUrl, when set, points the provider at it instead of PRITUNL_URL.
func testPritunlServerOnlineConfig(providerUrl, name string, octet, port int) string {
	provider := ""
	if providerUrl != "" {
		provider = fmt.Sprintf(`
provider "pritunl" {
  url = %q
}
`, providerUrl)
	}

	return provider + fmt.Sprintf(`
resource "pritunl_organization" "test" {
  name = "%[1]s-org"
}

resource "pritunl_server" "test" {
  name     = %[1]q
  network  = "10.86.%[3]d.0/24"
  port     = %[2]d
  protocol = "udp"

  organization_ids = [pritunl_organization.test.id]
  status           = "online"
}
`, name, port, octet)
}

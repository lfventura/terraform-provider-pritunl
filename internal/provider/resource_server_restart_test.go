package provider

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

// outageProxy forwards TCP to a target and can drop out for a while: it
// closes every connection and stops listening (connections are refused),
// then listens again on the same address.
type outageProxy struct {
	target string
	addr   string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	done  bool
	back  chan struct{}
}

func newOutageProxy(t *testing.T, target string) *outageProxy {
	t.Helper()

	p := &outageProxy{target: target, conns: map[net.Conn]struct{}{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p.addr = ln.Addr().String()
	p.ln = ln
	go p.serve(ln)
	t.Cleanup(p.close)

	return p
}

func (p *outageProxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}

		upstream, err := net.Dial("tcp", p.target)
		if err != nil {
			conn.Close()
			continue
		}

		p.mu.Lock()
		p.conns[conn], p.conns[upstream] = struct{}{}, struct{}{}
		p.mu.Unlock()

		go func() {
			io.Copy(upstream, conn)
			upstream.Close()
		}()
		go func() {
			io.Copy(conn, upstream)
			conn.Close()
		}()
	}
}

// outage refuses and drops everything for d, then resumes. back is closed
// when the proxy listens again.
func (p *outageProxy) outage(d time.Duration) {
	p.mu.Lock()
	p.ln.Close()
	for conn := range p.conns {
		conn.Close()
	}
	p.conns = map[net.Conn]struct{}{}
	p.back = make(chan struct{})
	back := p.back
	p.mu.Unlock()

	go func() {
		time.Sleep(d)

		p.mu.Lock()
		defer p.mu.Unlock()
		if p.done {
			return
		}

		var ln net.Listener
		var err error
		for i := 0; i < 50; i++ {
			if ln, err = net.Listen("tcp", p.addr); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			return
		}
		p.ln = ln
		go p.serve(ln)
		close(back)
	}()
}

func (p *outageProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.done = true
	p.ln.Close()
	for conn := range p.conns {
		conn.Close()
	}
}

// outageClient is the API client of the provider under test: it talks to
// the Pritunl instance through the proxy, and the proxy drops out right
// after the stop request went through.
type outageClient struct {
	pritunl.Client
	afterStop func()
}

func (c *outageClient) StopServer(id string) error {
	err := c.Client.StopServer(id)
	if err == nil {
		c.afterStop()
	}

	return err
}

func (c *outageClient) ForRestart(ctx context.Context) pritunl.Client {
	return c.Client.(pritunl.RestartScoper).ForRestart(ctx)
}

func waitServerStatus(t *testing.T, id, want string, within time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(within)
	for {
		server, err := testClient.GetServer(id)
		if err == nil && server.Status == want {
			return server.Status
		}
		if time.Now().After(deadline) {
			if err != nil {
				t.Fatalf("server %s never reached %q: %s", id, want, err)
			}
			t.Fatalf("server %s is %q, want %q", id, server.Status, want)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func restartLabServer(t *testing.T, port int) (serverId, orgId string) {
	t.Helper()

	org, err := testClient.CreateOrganization("tfacc-restart-org")
	if err != nil {
		t.Fatal(err)
	}

	cfg := func(name string) map[string]interface{} {
		return map[string]interface{}{
			"name":             name,
			"port":             port,
			"organization_ids": []interface{}{org.ID},
			"status":           pritunl.ServerStatusOnline,
		}
	}

	d := schema.TestResourceDataRaw(t, resourceServer().Schema, cfg("tfacc-restart"))
	if diags := resourceCreateServer(context.Background(), d, &providerMeta{Client: testClient}); diags.HasError() {
		testClient.DeleteOrganization(org.ID)
		t.Fatalf("creating the server: %v", diags)
	}

	t.Cleanup(func() {
		testClient.StopServer(d.Id())
		testClient.DeleteServer(d.Id())
		testClient.DeleteOrganization(org.ID)
	})

	waitServerStatus(t, d.Id(), pritunl.ServerStatusOnline, 60*time.Second)

	return d.Id(), org.ID
}

func restartLabUpdate(t *testing.T, proxy *outageProxy, id, orgId string, port int, window, outage time.Duration) (time.Duration, error) {
	t.Helper()

	proxied := pritunl.NewClient("https://"+proxy.addr+"/", "tfacctest_token", "tfacctest_secret", true)
	meta := &providerMeta{
		Client:              &outageClient{Client: proxied, afterStop: func() { proxy.outage(outage) }},
		restartRetryTimeout: window,
	}

	d := schema.TestResourceDataRaw(t, resourceServer().Schema, map[string]interface{}{
		"name":             "tfacc-restart-renamed",
		"port":             port,
		"organization_ids": []interface{}{orgId},
		"status":           pritunl.ServerStatusOnline,
	})
	d.SetId(id)

	began := time.Now()
	diags := resourceUpdateServer(context.Background(), d, meta)
	elapsed := time.Since(began)

	if diags.HasError() {
		msgs := make([]string, 0, len(diags))
		for _, dg := range diags {
			msgs = append(msgs, dg.Summary)
		}
		return elapsed, &labError{strings.Join(msgs, "\n")}
	}

	return elapsed, nil
}

type labError struct{ msg string }

func (e *labError) Error() string { return e.msg }

// The client reaches the API through a proxy that drops every connection and
// refuses new ones for 15s right after the server was stopped. The update
// retries on fresh connections and brings the server back online.
func TestAccServerRestartSurvivesAnOutage(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test: TF_ACC is not set")
	}
	preCheck(t)

	const port = 15001
	id, orgId := restartLabServer(t, port)
	proxy := newOutageProxy(t, "127.0.0.1:443")

	elapsed, err := restartLabUpdate(t, proxy, id, orgId, port, time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("update failed after %s: %s", elapsed, err)
	}
	if elapsed < 10*time.Second {
		t.Errorf("update took %s: the outage did not affect it", elapsed)
	}
	t.Logf("update succeeded after %s", elapsed)

	waitServerStatus(t, id, pritunl.ServerStatusOnline, 60*time.Second)

	server, err := testClient.GetServer(id)
	if err != nil {
		t.Fatal(err)
	}
	if server.Name != "tfacc-restart-renamed" {
		t.Errorf("the change was not applied, name = %q", server.Name)
	}
}

// An outage longer than the window fails the update; once the proxy is back
// a direct start brings the server online, so nothing was left corrupted.
func TestAccServerRestartOutageBeyondWindow(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("acceptance test: TF_ACC is not set")
	}
	preCheck(t)

	const port = 15002
	id, orgId := restartLabServer(t, port)
	proxy := newOutageProxy(t, "127.0.0.1:443")

	elapsed, err := restartLabUpdate(t, proxy, id, orgId, port, 5*time.Second, 20*time.Second)
	if err == nil {
		t.Fatalf("update succeeded after %s although the outage outlasted the window", elapsed)
	}
	t.Logf("update failed after %s: %s", elapsed, err)

	// the update calls and the start each have a 5s window of their own
	if elapsed < 9*time.Second {
		t.Errorf("update failed after %s, want at least two windows of 5s", elapsed)
	}

	if !strings.Contains(err.Error(), "restart_retry_timeout") {
		t.Errorf("the error does not report the window: %s", err)
	}
	if !strings.Contains(err.Error(), "starting the server again failed too") {
		t.Errorf("the error does not report the failed start: %s", err)
	}

	<-proxy.back
	if err := testClient.StartServer(id); err != nil {
		t.Fatalf("direct start: %s", err)
	}
	waitServerStatus(t, id, pritunl.ServerStatusOnline, 60*time.Second)
}

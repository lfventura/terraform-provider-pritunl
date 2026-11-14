package pritunl

import (
	"context"
	"fmt"
	"time"
)

// What a start of a server proves, read from the Pritunl source
// (pritunl/server/server.py, Server.start, and pritunl/server/instance.py):
//
//   - The start marks the server online in the database as its first step,
//     before any instance runs, then waits for an instance to report that it
//     started: up to 4 seconds and then up to vpn.op_timeout (25 seconds by
//     default). When none does, it marks the server offline again and fails
//     with "Server start timed out"; the instance that comes up late finds its
//     startup document gone and stops ("Startup doc lost, stopping server").
//   - A start of a server whose status is not offline returns 200 at once
//     without doing anything: a second start sent while the first one still
//     waits is answered 200 even when the first one then fails.
//   - A start of a server whose DH parameters are not generated yet only
//     queues their generation and returns 200; the server reads "pending".
//
// So neither a 200 nor an "online" read right after the start proves that the
// server runs. StartServerOnline sends the start once at a time and reads the
// server back until it is online in a way that cannot be reverted.
var (
	// DefaultServerStartTimeout is how long StartServerOnline keeps trying when
	// the caller gives no window.
	DefaultServerStartTimeout = 5 * time.Minute

	// serverStartSettle is how long a server must have been online, by the
	// uptime Pritunl reports, before a start whose answer was lost is taken as
	// done: by then the start Pritunl was running has either seen its instance
	// come up or put the server back offline. It covers the 4 seconds plus the
	// default 25 second vpn.op_timeout of Pritunl with a margin.
	serverStartSettle = 40 * time.Second

	// serverStartPollInterval is the pause between two reads of the server.
	serverStartPollInterval = 2 * time.Second
)

// StartServerOnline starts a server and returns once Pritunl runs it, or an
// error that names the server once window has passed without that.
//
// The server counts as running when it reads "online" and either every start
// of this call was answered and the last one with 200, or it has been online
// longer than serverStartSettle, the time Pritunl takes at most to revert a
// start that got no instance up. After a start whose answer was lost, only the
// settle time confirms: that request may begin in Pritunl at any later moment,
// and a start sent while it runs is answered 200 without doing anything.
//
// A start that fails transiently (the connection drops or is refused, a 5xx
// that is not an error document of the API, "Server start timed out"
// included) leaves its outcome unknown: the server is read back, and started
// again once it reads "offline", which is also what happens when a server
// that was started stops again. A server that reads "pending" waits for its
// DH parameters and is started once they are there. A failure that is not
// transient (an error document of the API, a 4xx, a certificate that does not
// verify, the context ending) is returned at once.
//
// window bounds the whole call: every request is bound to it, so a request
// that hangs or a read the transport retries through a restart of the web
// server ends when it closes, and nothing is sent once it has closed. ctx
// ending (Terraform interrupted, or the timeout of the resource) ends the call
// as well.
func StartServerOnline(ctx context.Context, c Client, serverId string, window time.Duration) error {
	if window <= 0 {
		window = DefaultServerStartTimeout
	}
	deadline := time.Now().Add(window)
	windowCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	progress := serverStartProgress{id: serverId, window: window}

	// stopped tells why a request failed when the call itself has to end: ctx
	// ended, or the window closed under the request
	stopped := func() error {
		if ctx.Err() != nil {
			return progress.fail(fmt.Sprintf("interrupted: %s", ctx.Err()), ctx.Err())
		}
		if !time.Now().Before(deadline) {
			return progress.fail("", nil)
		}
		return nil
	}

	// unsure is set once a start of this call failed transiently: its request
	// may still be on its way to Pritunl, or queued there, and begin after any
	// read, so no later 200 proves anything and only the settle time confirms
	// the server for the rest of the call. confirmed is set when a start was
	// answered 200 before that ever happened.
	needStart := true
	unsure := false
	confirmed := false

	for {
		if needStart {
			needStart = false
			progress.starts++

			err := c.StartServerContext(windowCtx, serverId)
			switch {
			case err == nil:
				confirmed = !unsure
			case stopped() != nil:
				return stopped()
			case isTransientReconciliation(err):
				unsure = true
				confirmed = false
				progress.lastErr = err
			default:
				return progress.fail(fmt.Sprintf("the start was refused: %s", err), err)
			}
		}

		server, err := c.GetServerContext(windowCtx, serverId)
		switch {
		case err == nil:
			progress.observe(server)

			switch server.Status {
			case ServerStatusOnline:
				if confirmed || time.Duration(server.Uptime)*time.Second >= serverStartSettle {
					return nil
				}
			case ServerStatusOffline:
				// no start runs right now: Pritunl marks the server online as
				// the first step of a start and offline again when it fails
				needStart = true
				confirmed = false
			default:
				// pending: the DH parameters are being generated, and the
				// server is started once it reads offline
				confirmed = false
			}
		case stopped() != nil:
			return stopped()
		case isTransientReconciliation(err):
			progress.lastErr = err
		default:
			return progress.fail(fmt.Sprintf("the server could not be read back: %s", err), err)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return progress.fail("", nil)
		}

		timer := time.NewTimer(minDuration(serverStartPollInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return stopped()
		case <-timer.C:
		}

		// the pause may end exactly when the window closes: nothing more is
		// sent then
		if !time.Now().Before(deadline) {
			return progress.fail("", nil)
		}
	}
}

// serverStartProgress is what StartServerOnline saw, for its error.
type serverStartProgress struct {
	id      string
	name    string
	window  time.Duration
	status  string
	uptime  int
	starts  int
	lastErr error
}

func (p *serverStartProgress) observe(server *Server) {
	if server.Name != "" {
		p.name = server.Name
	}
	p.status = server.Status
	p.uptime = server.Uptime
}

// fail builds the error of a server that is not running: reason is why the
// call stopped before the window closed, empty when it closed, and cause the
// error it stopped on, if any.
func (p *serverStartProgress) fail(reason string, cause error) error {
	server := fmt.Sprintf("server %s", p.id)
	if p.name != "" {
		server = fmt.Sprintf("server %q (%s)", p.name, p.id)
	}

	seen := "it was never read back"
	switch {
	case p.status == ServerStatusOnline:
		seen = fmt.Sprintf("Pritunl last reported it online for %ds, short of the %s a start may take to fail", p.uptime, serverStartSettle)
	case p.status != "":
		seen = fmt.Sprintf("Pritunl last reported it %s", p.status)
	}

	message := fmt.Sprintf("%s is not online after %d start request(s)", server, p.starts)
	if reason == "" {
		message = fmt.Sprintf("%s is not online after %s and %d start request(s)", server, p.window, p.starts)
	}
	message += "; " + seen
	if reason != "" {
		message += "; " + reason
	} else if p.lastErr != nil {
		message += fmt.Sprintf("; the last failure: %s", p.lastErr)
	}

	return &ServerStartError{ServerId: p.id, ServerName: p.name, Status: p.status, message: message, cause: cause}
}

// ServerStartError reports a server that StartServerOnline could not confirm
// as running.
type ServerStartError struct {
	ServerId   string
	ServerName string
	// Status is the status Pritunl last reported, empty when it was never read
	Status  string
	message string
	cause   error
}

func (e *ServerStartError) Error() string { return e.message }
func (e *ServerStartError) Unwrap() error { return e.cause }

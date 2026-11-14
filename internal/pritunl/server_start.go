package pritunl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// What a start of a server proves, read from the Pritunl source
// (pritunl/server/server.py, Server.start and Server.generate_dh_param,
// pritunl/queues/dh_params.py, pritunl/server/instance.py), the same in 1.32
// and 1.34:
//
//   - A server reads "pending" until its DH parameters are stored. Pritunl
//     keeps a pool of server_pool_size (3 by default) pre-generated sets; a
//     server created while the pool is empty waits for a set generated for it.
//   - A start of a pending server returns 200 without starting anything. When
//     no generation for the server is queued, it takes a set out of the pool
//     into memory and never stores it, so the set is lost and the server stays
//     pending. A pending server is therefore never sent a start.
//   - The start marks the server online in the database as its first step,
//     before any instance runs, then waits for an instance to report that it
//     started: up to 4 seconds and then up to vpn.op_timeout (25 seconds by
//     default). When none does, it marks the server offline again and fails
//     with "Server start timed out", answered as a 500 without an error
//     document of the API.
//   - A start of a server whose status is not offline returns 200 at once
//     without doing anything: a second start sent while the first one still
//     waits is answered 200 even when the first one then fails.
//
// So neither a 200 nor an "online" read right after the start proves that the
// server runs. StartServerOnline waits for the DH parameters without sending
// anything, sends the start once at a time and reads the server back until it
// is online in a way that cannot be reverted.
var (
	// DefaultServerStartTimeout is how long StartServerOnline keeps trying to
	// start a server that has its DH parameters when the caller gives no
	// window.
	DefaultServerStartTimeout = 5 * time.Minute

	// DefaultServerDhTimeout is how long StartServerOnline waits for a pending
	// server to get its DH parameters when the caller gives no window.
	DefaultServerDhTimeout = 10 * time.Minute

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
// error that names the server.
//
// It reads the server first. While the server reads "pending" it sends no
// start and waits, up to dhWindow from the beginning of the call, for the DH
// parameters; when the server still reads pending then, it returns a
// *ServerDhPendingError. Once the server has them, the start has its own
// window, starting then. A server that already reads online is not started:
// it is confirmed by the settle time, as after a start that got no answer.
//
// The server counts as running when it reads "online" and either every start
// of this call was answered and the last one with 200, or it has been online
// longer than serverStartSettle, the time Pritunl takes at most to revert a
// start that got no instance up. After a start whose answer never came (the
// connection dropped, or pritunl-web answered 502, 503 or 504 for the
// backend), only the settle time confirms: that request may begin in Pritunl
// at any later moment, and a start sent while it runs is answered 200 without
// doing anything.
//
// A start Pritunl answered with a 5xx that is not an error document of the API
// ("Server start timed out") is over: the server is read back and started
// again once it reads offline, which is also what happens when a server that
// was started stops again. A start that got no answer is handled the same way,
// under the settle rule above. A failure that is not transient (an error
// document of the API, a 4xx, a certificate that does not verify, the context
// ending) is returned at once.
//
// Each window bounds its phase: every request is bound to it, so a request
// that hangs or a read the transport retries through a restart of the web
// server ends when it closes, and nothing is sent once it has closed. ctx
// ending (Terraform interrupted, or the timeout of the resource) ends the call
// as well.
func StartServerOnline(ctx context.Context, c Client, serverId string, window, dhWindow time.Duration) error {
	if window <= 0 {
		window = DefaultServerStartTimeout
	}
	if dhWindow <= 0 {
		dhWindow = DefaultServerDhTimeout
	}

	progress := serverStartProgress{id: serverId, window: window}

	first, err := waitServerDhParams(ctx, c, &progress, dhWindow)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(window)
	windowCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

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

	// unsure is set once a start of this call got no answer: its request may
	// still be on its way to Pritunl, or queued there, and begin after any
	// read, so no later 200 proves anything and only the settle time confirms
	// the server for the rest of the call. It is also set when the server
	// already reads online before this call sent anything: a start of someone
	// else may still be waiting for its instance, and a start sent now would
	// be answered 200 without doing anything. confirmed is set when a start
	// was answered 200 while unsure was not set.
	needStart := first.Status != ServerStatusOnline
	unsure := first.Status == ServerStatusOnline
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
			case isAnsweredStartFailure(err):
				// Pritunl ran the start and gave it up: the server is back
				// offline and nothing of this request is left in flight
				confirmed = false
				progress.lastErr = err
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
				// pending again: a start now would be lost, the server is
				// started once it reads offline
				confirmed = false
			}
		case stopped() != nil:
			return stopped()
		case isTransientReconciliation(err):
			progress.lastErr = err
		default:
			return progress.fail(fmt.Sprintf("the server could not be read back: %s", err), err)
		}

		if !pauseServerStart(ctx, deadline) {
			if ctx.Err() != nil {
				return stopped()
			}
			return progress.fail("", nil)
		}
	}
}

// waitServerDhParams reads the server until it no longer reads "pending",
// sending nothing else, for up to dhWindow. It returns the server read last.
func waitServerDhParams(ctx context.Context, c Client, progress *serverStartProgress, dhWindow time.Duration) (*Server, error) {
	deadline := time.Now().Add(dhWindow)
	dhCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	// readErr is the failure of the latest read, nil when it read the server:
	// only a server that read pending at the end of the window is reported as
	// one that never got its DH parameters
	var readErr error
	for {
		server, err := c.GetServerContext(dhCtx, progress.id)
		readErr = err
		switch {
		case err == nil:
			progress.observe(server)
			if server.Status != ServerStatusPending {
				return server, nil
			}
		case ctx.Err() != nil:
			return nil, progress.fail(fmt.Sprintf("interrupted: %s", ctx.Err()), ctx.Err())
		case !time.Now().Before(deadline):
			// the window closed under the read
		case isTransientReconciliation(err):
		default:
			return nil, progress.fail(fmt.Sprintf("the server could not be read back: %s", err), err)
		}

		if !pauseServerStart(ctx, deadline) {
			if ctx.Err() != nil {
				return nil, progress.fail(fmt.Sprintf("interrupted: %s", ctx.Err()), ctx.Err())
			}
			if readErr != nil {
				return nil, progress.fail(fmt.Sprintf("the server could not be read back while it was waited for: %s", readErr), readErr)
			}
			return nil, &ServerDhPendingError{ServerId: progress.id, ServerName: progress.name, Window: dhWindow}
		}
	}
}

// pauseServerStart waits for the next read: false when ctx ended or deadline
// came first, and then nothing more may be sent.
func pauseServerStart(ctx context.Context, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}

	timer := time.NewTimer(minDuration(serverStartPollInterval, remaining))
	select {
	case <-ctx.Done():
		timer.Stop()
		return false
	case <-timer.C:
	}

	// the pause may end exactly when the window closes
	return time.Now().Before(deadline)
}

// isAnsweredStartFailure reports whether a start failed with an answer of
// Pritunl itself: a 5xx other than the 502, 503 and 504 pritunl-web gives for
// a backend it could not reach, and not an error document of the API (a
// refusal). "Server start timed out" is one: Pritunl put the server back
// offline before answering, so the request is over.
func isAnsweredStartFailure(err error) bool {
	var apiError *ApiError
	if !errors.As(err, &apiError) {
		return false
	}

	var fatal fatalError
	if errors.As(err, &fatal) || isGatewayStatus(apiError.StatusCode) {
		return false
	}

	return apiError.StatusCode >= 500 && !isApiErrorBody([]byte(apiError.Body))
}

// isGatewayStatus reports the statuses pritunl-web answers for a backend it
// could not reach: the request may or may not have been served.
func isGatewayStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}

	return false
}

// ServerDhPendingError reports a server that still read "pending" when the DH
// window closed: Pritunl never stored DH parameters for it, and no start was
// sent.
type ServerDhPendingError struct {
	ServerId   string
	ServerName string
	Window     time.Duration
}

func (e *ServerDhPendingError) Error() string {
	server := fmt.Sprintf("server %s", e.ServerId)
	if e.ServerName != "" {
		server = fmt.Sprintf("server %q (%s)", e.ServerName, e.ServerId)
	}

	return fmt.Sprintf("%s still reads pending after %s: Pritunl never generated its DH parameters, so it was not started", server, e.Window)
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

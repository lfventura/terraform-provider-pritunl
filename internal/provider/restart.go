package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/lfventura/terraform-provider-pritunl/internal/pritunl"
)

const defaultRestartRetryTimeout = time.Minute

const restartRetryTimeoutDescription = "How long the calls that bring a stopped `pritunl_server` back keep retrying transient failures (network errors and every HTTP 5xx status), each retry on a fresh connection, as a Go duration greater than zero such as `90s`. The calls that finish an update share one window, the start has a window of its own, and no attempt outlives its window. Defaults to `1m`; also taken from the `PRITUNL_RESTART_RETRY_TIMEOUT` environment variable."

// The wait between two attempts starts at restartBackoffInitial, doubles and
// stops growing at restartBackoffMax. They are variables so tests run fast.
var (
	restartBackoffInitial = time.Second
	restartBackoffMax     = 10 * time.Second
)

// providerMeta is what the provider hands to every resource: the API client
// plus the provider level settings the resources need.
type providerMeta struct {
	pritunl.Client
	restartRetryTimeout time.Duration
}

// RestartRetryTimeout is the window in which the calls that bring a stopped
// server back keep retrying transient failures.
func (m *providerMeta) RestartRetryTimeout() time.Duration {
	return m.restartRetryTimeout
}

// parseRestartRetryTimeout reads the restart_retry_timeout argument: a Go
// duration, greater than zero.
func parseRestartRetryTimeout(value string) (time.Duration, error) {
	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("restart_retry_timeout %q is not a duration such as \"1m\" or \"90s\": %w", value, err)
	}

	if timeout <= 0 {
		return 0, fmt.Errorf("restart_retry_timeout must be greater than zero, got %q", value)
	}

	return timeout, nil
}

func validateRestartRetryTimeout(value interface{}, _ string) ([]string, []error) {
	if _, err := parseRestartRetryTimeout(value.(string)); err != nil {
		return nil, []error{err}
	}

	return nil, nil
}

// restartRetry runs the calls of a server restart: each one retries
// transient failures on a fresh connection until the deadline, and no single
// attempt outlives it.
type restartRetry struct {
	ctx      context.Context
	client   pritunl.Client
	window   time.Duration
	deadline time.Time
}

// newRestartRetry reads the window from the provider meta (the default when
// the meta carries none).
func newRestartRetry(ctx context.Context, meta interface{}) *restartRetry {
	window := defaultRestartRetryTimeout
	if m, ok := meta.(interface{ RestartRetryTimeout() time.Duration }); ok && m.RestartRetryTimeout() > 0 {
		window = m.RestartRetryTimeout()
	}

	client := meta.(pritunl.Client)
	if m, ok := meta.(*providerMeta); ok {
		client = m.Client
	}

	return (&restartRetry{ctx: ctx, client: client, window: window}).fresh()
}

// fresh returns a retrier with a new window starting now.
func (r *restartRetry) fresh() *restartRetry {
	return &restartRetry{ctx: r.ctx, client: r.client, window: r.window, deadline: time.Now().Add(r.window)}
}

// detached returns a retrier with a new window that keeps going when the
// context of the operation is cancelled: it brings a stopped server back
// even after the operator interrupted the run.
func (r *restartRetry) detached() *restartRetry {
	return &restartRetry{ctx: context.WithoutCancel(r.ctx), client: r.client, window: r.window, deadline: time.Now().Add(r.window)}
}

// attemptClient scopes the client to a single attempt on a fresh connection,
// bound to ctx, when the client supports it.
func (r *restartRetry) attemptClient(ctx context.Context) pritunl.Client {
	if scoper, ok := r.client.(pritunl.RestartScoper); ok {
		return scoper.ForRestart(ctx)
	}

	return r.client
}

// do runs op until it succeeds, fails with a non-transient error, the
// context ends or the window closes. It always makes at least one attempt,
// and every attempt is cut off when the window closes.
func (r *restartRetry) do(op func(c pritunl.Client) error) error {
	wait := restartBackoffInitial

	for {
		attemptCtx, cancel := context.WithDeadline(r.ctx, r.deadline)
		err := op(r.attemptClient(attemptCtx))
		cancel()
		if err == nil || !pritunl.IsTransient(err) || r.ctx.Err() != nil {
			return err
		}

		remaining := time.Until(r.deadline)
		if remaining <= 0 {
			return fmt.Errorf("still failing after the %s restart_retry_timeout: %w", r.window, err)
		}

		if wait > remaining {
			wait = remaining
		}

		timer := time.NewTimer(wait)
		select {
		case <-r.ctx.Done():
			timer.Stop()
			return err
		case <-timer.C:
		}

		wait *= 2
		if wait > restartBackoffMax {
			wait = restartBackoffMax
		}
	}
}

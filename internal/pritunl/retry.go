package pritunl

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"syscall"
	"time"
)

// Pritunl restarts its web server when the TLS certificate, its key or the
// listening port change, and for a fraction of a second every request in
// flight meets the restart: pritunl-web answers 502 with an empty body while
// the backend is gone, or the connection is reset or closed without an answer.
// A restart is also what a rolling upgrade or a briefly unreachable load
// balancer looks like to the client.
//
// Everything below tells such a transient failure from an answer of the API
// itself, and bounds how long a call keeps trying. The variables are what the
// unit tests shorten.
var (
	// transportRetryWindow bounds how long one call keeps retrying transient failures
	transportRetryWindow = 90 * time.Second
	// transportRetryFirstWait is the pause before the first retry; it doubles up to
	// transportRetryMaxWait
	transportRetryFirstWait = time.Second
	transportRetryMaxWait   = 10 * time.Second
)

// isTransientError reports whether a failed request failed because the
// connection to the web server was refused, reset, closed or timed out, which
// is how a restarting web server shows itself. A TLS certificate that does not
// verify, a name that does not resolve and a cancelled context are answers of
// their own, and retrying them only delays the failure.
func isTransientError(err error) bool {
	if err == nil {
		return false
	}

	var verifyErr *tls.CertificateVerificationError
	if errors.As(err, &verifyErr) {
		return false
	}

	// a deadline is reported as a timeout, and it is the caller's own
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return false
}

// isTransientStatus reports whether an HTTP answer is the web server being
// unavailable or failing rather than the API refusing the request: 502, 503
// and 504, and any other 5xx that is not an error document of the API. The API
// reports its own errors with a JSON body carrying an "error" code; a 5xx
// without one, empty or not, is the server or a proxy in front of it failing.
// Everything below 500 is the API's own answer: a client error without a body
// is not transient either, Pritunl answers a request body it cannot decode with
// an empty 400.
func isTransientStatus(status int, body []byte) bool {
	switch {
	case status < 500:
		return false
	case status == http.StatusBadGateway, status == http.StatusServiceUnavailable, status == http.StatusGatewayTimeout:
		return true
	}

	return !isApiErrorBody(body)
}

// isApiErrorBody reports whether a body is an error document of the Pritunl
// API, a JSON object with an "error" code.
func isApiErrorBody(body []byte) bool {
	var document struct {
		Error string `json:"error"`
	}

	return json.Unmarshal(body, &document) == nil && document.Error != ""
}

// transientResponse classifies an answer, reading the body only when the
// status alone does not decide, and hands the answer back with its body intact.
func transientResponse(resp *http.Response) (bool, *http.Response, error) {
	switch {
	case resp.StatusCode < 500:
		return false, resp, nil
	case resp.StatusCode == http.StatusBadGateway,
		resp.StatusCode == http.StatusServiceUnavailable,
		resp.StatusCode == http.StatusGatewayTimeout:
		return true, resp, nil
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return false, resp, err
	}

	return isTransientStatus(resp.StatusCode, body), resp, nil
}

// backoff is the pause schedule between two retries of one call.
type backoff struct {
	wait time.Duration
}

func newBackoff() *backoff {
	return &backoff{wait: transportRetryFirstWait}
}

// next returns the pause to take now and doubles the following one up to the
// cap.
func (b *backoff) next() time.Duration {
	wait := b.wait
	b.wait *= 2
	if b.wait > transportRetryMaxWait {
		b.wait = transportRetryMaxWait
	}

	return wait
}

// fatalError stops a retry loop at once: whatever raised it is not going to
// get better by waiting.
type fatalError struct {
	err error
}

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// attemptResult is what one attempt of a request came to.
type attemptResult struct {
	status int
	body   []byte
	err    error
}

func (r attemptResult) transient() bool {
	if r.err != nil {
		return isTransientError(r.err)
	}

	return isTransientStatus(r.status, r.body)
}

// retryCreate sends a request that creates something and survives the web
// server restarting under it.
//
// A create that failed transiently may still have been committed: the
// restart can swallow the answer of a request that was already served, and
// sending it again would create the object twice. So before every retry
// settled reads the state back and reports whether the object is already
// there, in which case the call is done and attempt is not repeated. attempt
// runs again only while the object is verifiably absent and the window is
// open. A failure that is not transient, the answer of the API included, ends
// the loop at once and is returned as it is, as is the last failure once the
// window closes.
//
// settled returning a transient error means the state could not be read right
// now, which is the restart again, and the loop goes on. Any other error, a
// fatalError or an answer of the API such as a refused login, stops the loop
// and is returned: retrying cannot help, and it is the one worth reading.
func retryCreate(attempt func() attemptResult, settled func() (bool, error)) (result attemptResult, adopted bool, err error) {
	deadline := time.Now().Add(transportRetryWindow)
	pause := newBackoff()

	for {
		result = attempt()
		if !result.transient() || !time.Now().Before(deadline) {
			return result, false, nil
		}

		for {
			if remaining := time.Until(deadline); remaining > 0 {
				time.Sleep(minDuration(pause.next(), remaining))
			}

			done, err := settled()
			if err == nil && done {
				return result, true, nil
			}
			if err != nil && !isTransientReconciliation(err) {
				return result, false, fmt.Errorf("the request failed (%s) and the state could not be read back to tell whether it was served: %w", result.describe(), err)
			}
			if !time.Now().Before(deadline) {
				return result, false, nil
			}
			if err == nil {
				break
			}
		}
	}
}

// describe says what a request came to, for the error of a call that cannot
// tell whether the request was served.
func (r attemptResult) describe() string {
	if r.err != nil {
		return r.err.Error()
	}

	return fmt.Sprintf("code=%d", r.status)
}

// isTransientReconciliation reports whether a failure to read the state back
// is the web server being unavailable, which is worth another try, rather than
// an answer that retrying cannot change: a refused login, a certificate that
// does not verify, a body that cannot be read.
func isTransientReconciliation(err error) bool {
	var fatal fatalError
	if errors.As(err, &fatal) {
		return false
	}

	var apiError *ApiError
	if errors.As(err, &apiError) {
		return isTransientStatus(apiError.StatusCode, []byte(apiError.Body))
	}

	return isTransientError(err)
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}

	return b
}

// createLocks serialises the creates that could be taken for one another.
var createLocks sync.Map

// lockCreate holds the lock of an identity, a kind of object with the name that
// tells it apart, until the returned function is called. Two creates of the same
// identity in flight at once are told apart by nothing but the order they run
// in: after one of them failed transiently, the object the other created would
// be taken for its own. The lock covers the whole sequence of a create, the
// baseline read included, so each create sees exactly the objects that exist
// when it starts and no other call of the provider creates one of its identity
// while it runs.
func lockCreate(identity string) func() {
	lock, _ := createLocks.LoadOrStore(identity, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()

	return mutex.Unlock
}

// pickCreated tells which object a create that failed transiently made, out of
// the ids that appeared since it was first sent and match what it asked for:
// none means it made nothing, one is the object, and several cannot be told
// apart, which is not settled by guessing.
func pickCreated(kind, name string, fresh []string) (string, bool, error) {
	switch len(fresh) {
	case 0:
		return "", false, nil
	case 1:
		return fresh[0], true, nil
	}

	return "", false, fatalError{fmt.Errorf("the request failed on a restart of the web server and %d new %s named %q exist now, which one it created cannot be told: %v", len(fresh), kind, name, fresh)}
}

// newIds returns the ids of live that are not among the known ones.
func newIds(known, live []string) []string {
	var fresh []string
	for _, id := range live {
		if !containsString(known, id) {
			fresh = append(fresh, id)
		}
	}

	return fresh
}

func containsString(list []string, value string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}

	return false
}

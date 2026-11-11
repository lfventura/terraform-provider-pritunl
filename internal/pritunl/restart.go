package pritunl

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// RestartScoper is implemented by a Client that can hand out a view of
// itself for the calls that finish a server restart: every request is a
// single attempt on a brand new connection (so the host name resolves again,
// which matters when the route to the API changes while the server restarts)
// and is bound to ctx. The retries belong to the caller.
type RestartScoper interface {
	ForRestart(ctx context.Context) Client
}

// ForRestart returns the restart view of the client.
func (c client) ForRestart(ctx context.Context) Client {
	if c.restartHttpClient == nil {
		return c
	}

	scoped := c
	scoped.httpClient = c.restartHttpClient(ctx)

	return scoped
}

func newRestartHttpClient(ctx context.Context, baseUrl, apiToken, apiSecret string, insecure bool) *http.Client {
	underlyingTransport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
		DisableKeepAlives:     true,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: time.Minute,
	}

	return &http.Client{
		Transport: &transport{
			baseUrl:             baseUrl,
			apiToken:            apiToken,
			apiSecret:           apiSecret,
			underlyingTransport: underlyingTransport,
			single:              true,
			ctx:                 ctx,
		},
	}
}

// IsTransient reports whether err is a failure that may clear by itself and
// is worth another attempt: the connection to the API could not be made or
// broke (resolution, dial, refused, reset, EOF, timeouts, TLS handshake) or
// the API answered with a 5xx status (a gateway error, or Pritunl itself
// still busy right after a server stop). A refusal by the API (4xx,
// validation errors) is not transient.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	var apiError *ApiError
	if errors.As(err, &apiError) {
		return apiError.StatusCode >= http.StatusInternalServerError
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	// a url.Error is a net.Error whatever it wraps (a certificate refusal
	// included), so only a timeout or a failed network operation counts.
	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		return true
	}

	var opError *net.OpError
	if errors.As(err, &opError) {
		return true
	}

	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return true
	}

	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return true
	}

	return strings.Contains(err.Error(), "TLS handshake")
}

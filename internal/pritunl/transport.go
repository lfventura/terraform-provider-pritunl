package pritunl

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type transport struct {
	underlyingTransport http.RoundTripper
	apiToken            string
	apiSecret           string
	baseUrl             string
}

// RoundTrip signs and sends the request, retrying transient failures until the
// retry window closes: a connection that is refused, reset, closed or timed out
// and the answers of a web server that is unavailable (see isTransientError and
// isTransientStatus). The answers of the API itself, error bodies included, are
// never retried. Every attempt signs the request again (the nonce is single-use
// server side) and replays the body through GetBody; a request whose body
// cannot be replayed is sent only once.
//
// A POST is replayed only when the attempt provably never left, a dial
// failure: one that reached the server may have committed before the answer
// was lost, and replaying it would create a duplicate. The callers that create
// something retry those themselves after reading the state back (see
// retryCreate).
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == "" {
		u, err := url.Parse(t.baseUrl)
		if err != nil {
			return nil, err
		}

		u.Path = path.Join(u.Path, req.URL.Path)
		req.URL = u
	}

	deadline := time.Now().Add(transportRetryWindow)
	pause := newBackoff()

	for {
		if req.Body != nil && req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}

		t.sign(req)

		resp, err := t.underlyingTransport.RoundTrip(req)

		var retriable bool
		switch {
		case req.Method == http.MethodPost:
			retriable = isDialError(err) && isTransientError(err)
		case err != nil:
			retriable = isTransientError(err)
		default:
			var readErr error
			retriable, resp, readErr = transientResponse(resp)
			if readErr != nil {
				// the body broke off while it was read: the answer is lost,
				// like a connection that dropped
				err = readErr
				resp = nil
				retriable = isTransientError(readErr)
			}
		}

		replayable := req.Body == nil || req.GetBody != nil
		if !retriable || !replayable || !time.Now().Before(deadline) {
			return resp, err
		}
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(pause.next()):
		}
	}
}

// isDialError reports whether the request never reached the server: the
// connection itself could not be established, so nothing was delivered and
// a replay cannot duplicate anything.
func isDialError(err error) bool {
	var opErr *net.OpError
	return errors.As(err, &opErr) && opErr.Op == "dial"
}

func (t *transport) sign(req *http.Request) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	timestampNano := strconv.FormatInt(time.Now().UnixNano(), 10)

	nonceMac := hmac.New(md5.New, []byte(t.apiSecret))
	nonceMac.Write([]byte(strings.Join([]string{timestampNano, req.URL.Path, t.apiToken}, "")))
	nonce := fmt.Sprintf("%x", nonceMac.Sum(nil))
	authString := strings.Join([]string{t.apiToken, timestamp, nonce, strings.ToUpper(req.Method), req.URL.Path}, "&")

	mac := hmac.New(sha256.New, []byte(t.apiSecret))
	mac.Write([]byte(authString))
	signature := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("Auth-Token", t.apiToken)
	req.Header.Set("Auth-Timestamp", timestamp)
	req.Header.Set("Auth-Nonce", nonce)
	req.Header.Set("Auth-Signature", signature)

	req.Header.Set("Content-Type", "application/json")
}

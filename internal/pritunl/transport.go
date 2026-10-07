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

const (
	// Pritunl restarts its web server right after some settings writes (a
	// TLS certificate or port change), which surfaces to every concurrent
	// caller as a transport error or a 5xx answer for a few seconds.
	transportRetryWindow  = 75 * time.Second
	transportRetryMaxWait = 10 * time.Second
)

// RoundTrip signs and sends the request, retrying transient failures —
// transport errors and 5xx answers — until the retry window closes. Every
// attempt signs the request again (the nonce is single-use server side)
// and replays the body through GetBody; a request whose body cannot be
// replayed is sent only once.
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
	wait := time.Second

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

		retriable := err != nil || resp.StatusCode >= 500
		if req.Method == http.MethodPost {
			// A POST that reached the server may have committed before the
			// answer was lost, and replaying it would create a duplicate.
			// Only an attempt that provably never left - a dial failure -
			// retries.
			retriable = isDialError(err)
		}
		replayable := req.Body == nil || req.GetBody != nil
		if !retriable || !replayable || time.Now().After(deadline) {
			return resp, err
		}
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(wait):
		}
		if wait < transportRetryMaxWait {
			wait *= 2
			if wait > transportRetryMaxWait {
				wait = transportRetryMaxWait
			}
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

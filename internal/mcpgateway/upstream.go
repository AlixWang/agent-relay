package mcpgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Upstream HTTP client shared by service packages (design v5 §3.1). Writes are
// never retried; reads may retry once on a transient failure. Error strings
// deliberately never include the request URL: the Bing API carries its key in
// the query string.
type Upstream struct {
	client    *http.Client
	userAgent string
}

var (
	ErrUpstreamTimeout = errors.New("upstream timeout")
	ErrUpstreamNetwork = errors.New("upstream network error")
	ErrUpstreamTooBig  = errors.New("upstream response too large")
)

const maxUpstreamBytes = 4 << 20

// NoRedirectClient returns an HTTP client with redirects disabled so
// Authorization headers, query-string API keys, and OAuth form bodies are never
// forwarded on 3xx responses.
func NoRedirectClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func NewUpstream(timeout time.Duration) *Upstream {
	return &Upstream{
		client:    NoRedirectClient(timeout),
		userAgent: "relay-mcp/1.0",
	}
}

// WithClient lets tests inject an httptest client. The timeout is already
// encoded in the supplied client.
func (u *Upstream) WithClient(c *http.Client) *Upstream {
	if c != nil {
		u.client = c
	}
	return u
}

// Response is the bounded upstream response body.
type Response struct {
	Status int
	Body   []byte
	Header http.Header
}

// Do performs one HTTP request. body is JSON-encoded when non-nil.
func (u *Upstream) Do(ctx context.Context, method, rawURL string, body any, headers map[string]string) (*Response, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode upstream body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		// Do not echo rawURL: it may contain the Bing apikey.
		return nil, fmt.Errorf("build upstream request")
	}
	req.Header.Set("User-Agent", u.userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := u.client.Do(req)
	if err != nil {
		if IsTimeout(err) {
			return nil, ErrUpstreamTimeout
		}
		return nil, ErrUpstreamNetwork
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, maxUpstreamBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		if IsTimeout(err) || IsTimeout(ctx.Err()) {
			return nil, ErrUpstreamTimeout
		}
		return nil, ErrUpstreamNetwork
	}
	if len(data) > maxUpstreamBytes {
		return nil, ErrUpstreamTooBig
	}
	return &Response{Status: resp.StatusCode, Body: data, Header: resp.Header}, nil
}

// IsTimeout reports whether err represents a context cancellation/deadline or
// transport timeout.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	type timeout interface{ Timeout() bool }
	var t timeout
	return errors.As(err, &t) && t.Timeout()
}

// DoRead is the read-only variant: one retry on network errors and on
// 429/500/502/503/504. Writes must use Do exactly once.
func (u *Upstream) DoRead(ctx context.Context, method, rawURL string, body any, headers map[string]string) (*Response, error) {
	resp, err := u.Do(ctx, method, rawURL, body, headers)
	if err == nil && !retryableStatus(resp.Status) {
		return resp, nil
	}
	if err != nil && !errors.Is(err, ErrUpstreamNetwork) && !errors.Is(err, ErrUpstreamTimeout) {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ErrUpstreamTimeout
	case <-time.After(150 * time.Millisecond):
	}
	return u.Do(ctx, method, rawURL, body, headers)
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// UpstreamError maps a non-2xx upstream status to a safe tool error. The body
// is not returned: it may be large and is not needed by the agent.
func UpstreamError(status int) *Error {
	code := CodeUpstreamError
	msg := fmt.Sprintf("upstream returned HTTP %d", status)
	if status == 0 {
		msg = "upstream request failed"
	}
	return &Error{
		Code: code, Message: msg, HTTPStatus: 200,
		Details: map[string]any{"upstream_status": status},
	}
}

// UpstreamFailure maps a transport error to upstream_timeout/upstream_error.
func UpstreamFailure(err error) *Error {
	if errors.Is(err, ErrUpstreamTimeout) {
		return &Error{Code: CodeUpstreamTimeout, Message: "upstream timed out", HTTPStatus: 200}
	}
	if errors.Is(err, ErrUpstreamTooBig) {
		return &Error{Code: CodeUpstreamError, Message: "upstream response too large", HTTPStatus: 200}
	}
	return &Error{Code: CodeUpstreamError, Message: "upstream request failed", HTTPStatus: 200}
}

// DecodeUpstreamJSON decodes a bounded upstream body. A decode failure is an
// upstream_error, never a 500.
func DecodeUpstreamJSON(resp *Response, dst any) *Error {
	if len(resp.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(resp.Body, dst); err != nil {
		return &Error{Code: CodeUpstreamError, Message: "upstream returned invalid JSON", HTTPStatus: 200}
	}
	return nil
}

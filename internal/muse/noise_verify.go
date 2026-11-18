package muse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// noiseVerifyBudget is the hard ceiling for one whole verification: token
	// chain + at most three dial/handshake/RPC attempts. It is not per attempt.
	noiseVerifyBudget = 25 * time.Second
	noiseTokenTimeout = 4 * time.Second
	noiseDialTimeout  = 6 * time.Second
	noiseHandshakeTMO = 6 * time.Second
	noiseRPCTimeout   = 4 * time.Second
)

// verifyNoise proves that a 200 wake request actually reached a live VM:
// take one hatch token, then up to three dials completing a Noise XX handshake
// plus one read-only RPC. Token is never re-fetched for dial retries; the only
// exception is a 401/403 at a token endpoint, which gets one session refresh
// and one retry of that step. Cookies are merged into res on every response so
// renew can seal the rotated jar even when verification fails.
func (s *Service) verifyNoise(ctx context.Context, res *renewResult, sess *sessionResponse) error {
	start := time.Now()
	budget, cancel := context.WithTimeout(ctx, noiseVerifyBudget)
	defer cancel()

	token, err := s.fetchHatchToken(budget, res, sess)
	if err != nil {
		return annotateNoiseAttempt(err, 1, start)
	}

	var last *noiseError
	attempts := 0
	for attempt := 1; attempt <= noiseMaxAttempts; attempt++ {
		attempts = attempt
		if attempt > 1 {
			delay := time.Duration(attempt-1) * time.Second // 1s, then 2s
			timer := time.NewTimer(delay)
			select {
			case <-budget.Done():
				timer.Stop()
				if last == nil {
					last = newNoiseError("dial", noiseNetworkClass(budget.Err()), true, budget.Err())
				}
				return annotateNoiseAttempt(last, attempt, start)
			case <-timer.C:
			}
		}

		dialCtx, dialCancel := context.WithTimeout(budget, noiseDialTimeout)
		conn, derr := dialNoiseConn(dialCtx, s.client, s.gatewayURL, sess.VMID, token)
		dialCancel()
		if derr != nil {
			last = noiseAsError(derr)
			if !last.Retryable || attempt == noiseMaxAttempts {
				break
			}
			continue
		}

		herr := conn.handshake(budget, noiseHandshakeTMO)
		if herr == nil {
			herr = verifyNoiseRPC(budget, conn)
		}
		conn.close()
		if herr == nil {
			return nil
		}
		last = noiseAsError(herr)
		if !last.Retryable || attempt == noiseMaxAttempts {
			break
		}
	}
	if last == nil {
		last = newNoiseError("dial", "unknown", false, nil)
	}
	return annotateNoiseAttempt(last, attempts, start)
}

func noiseAsError(err error) *noiseError {
	if err == nil {
		return nil
	}
	var ne *noiseError
	if errors.As(err, &ne) {
		return ne
	}
	return newNoiseError("dial", "internal", false, err)
}

func annotateNoiseAttempt(err error, attempt int, start time.Time) error {
	ne := noiseAsError(err)
	if ne == nil {
		return nil
	}
	ne.Attempt = attempt
	ne.Elapsed = time.Since(start)
	return ne
}

// fetchHatchToken runs POST /api/auth/check then POST /api/hatch/token, reusing
// the service HTTP client so proxy/TLS/timeout configuration stays in one
// place. vmAddress prefers /api/session's endpoint_url and only falls back to
// the per-VM hostname when it is absent or malformed.
func (s *Service) fetchHatchToken(ctx context.Context, res *renewResult, sess *sessionResponse) (string, error) {
	if sess == nil || strings.TrimSpace(sess.VMID) == "" {
		return "", newNoiseError("auth", "missing_vm_id", false, nil)
	}
	refreshUsed := false
	access, err := s.fetchTokenValue(ctx, res, "auth", "/api/auth/check", nil, "",
		"access_token", &refreshUsed)
	if err != nil {
		return "", err
	}
	vmAddress := noiseVMAddress(sess)
	body, err := json.Marshal(map[string]string{"vmAddress": vmAddress, "vmName": sess.VMID})
	if err != nil {
		return "", newNoiseError("token", "encode", false, err)
	}
	return s.fetchTokenValue(ctx, res, "token", "/api/hatch/token", body, access,
		"token", &refreshUsed)
}

// fetchTokenValue issues one token request and, on 401/403, performs at most
// one session refresh for this whole verification and retries the step once.
// Set-Cookie is merged even on errors, before the status is classified.
func (s *Service) fetchTokenValue(ctx context.Context, res *renewResult, stage, path string,
	body []byte, bearer, jsonField string, refreshUsed *bool) (string, error) {

	call := func() (*http.Response, []byte, error) {
		reqCtx, cancel := context.WithTimeout(ctx, noiseTokenTimeout)
		defer cancel()
		return s.doNoiseRequest(reqCtx, res, path, body, bearer)
	}
	resp, raw, err := call()
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusOK {
		return parseTokenField(stage, jsonField, raw)
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return "", noiseTokenStatusError(stage, resp, s.now())
	}
	if *refreshUsed {
		return "", noiseTokenStatusError(stage, resp, s.now())
	}
	*refreshUsed = true
	if err := s.refreshNoiseSession(ctx, res); err != nil {
		return "", err
	}
	resp, raw, err = call()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", noiseTokenStatusError(stage, resp, s.now())
	}
	return parseTokenField(stage, jsonField, raw)
}

func noiseTokenStatusError(stage string, resp *http.Response, now time.Time) *noiseError {
	cause := newMuseHTTPError("noise "+stage, resp, now)
	ne := newNoiseError(stage, noiseStatusClass(resp.StatusCode), false, cause)
	return ne
}

func parseTokenField(stage, field string, raw []byte) (string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", newNoiseError(stage, "non_json", false, nil)
	}
	var value string
	if err := json.Unmarshal(payload[field], &value); err != nil || strings.TrimSpace(value) == "" {
		return "", newNoiseError(stage, "missing_"+field, false, nil)
	}
	return value, nil
}

// refreshNoiseSession re-reads /api/session with the merged cookie jar and
// merges any further rotation. It deliberately does not touch AuthState: the
// caller is inside renew, whose session result already stands.
func (s *Service) refreshNoiseSession(ctx context.Context, res *renewResult) error {
	reqCtx, cancel := context.WithTimeout(ctx, noiseTokenTimeout)
	defer cancel()
	sess, set, err := s.getSession(reqCtx, res.Cookies)
	mergeSetCookies(res.Cookies, res.Exp, set, s.now().Unix())
	if err != nil {
		return newNoiseSessionError(err)
	}
	if sess.Status != "assigned" {
		return newNoiseError("auth", "not_assigned", false, nil)
	}
	return nil
}

func newNoiseSessionError(err error) *noiseError {
	if errors.Is(err, ErrUnauthorized) {
		return newNoiseError("auth", "401", false, err)
	}
	var httpErr *MuseHTTPError
	if errors.As(err, &httpErr) {
		return newNoiseError("auth", noiseStatusClass(httpErr.StatusCode), false, err)
	}
	return newNoiseError("auth", "session", false, err)
}

// doNoiseRequest is a minimal JSON/empty-body POST with the browser headers
// Muse expects. It reuses s.client (proxy, TLS, CheckRedirect) and caps the
// body at 1 MiB like the rest of the muse package.
func (s *Service) doNoiseRequest(ctx context.Context, res *renewResult, path string,
	body []byte, bearer string) (*http.Response, []byte, error) {

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.baseURL+path, rdr)
	if err != nil {
		return nil, nil, newNoiseError("auth", "request", false, err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", s.baseURL)
	req.Header.Set("Referer", s.baseURL+"/")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	if len(res.Cookies) > 0 {
		req.Header.Set("Cookie", cookieHeader(res.Cookies))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, newNoiseError("auth", noiseNetworkClass(err), false, err)
	}
	defer resp.Body.Close()
	// Merge rotation before looking at the status: a 401/403 can also set a
	// fresh cookie, and the one allowed session refresh must use it.
	mergeSetCookies(res.Cookies, res.Exp, resp.Cookies(), s.now().Unix())
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, newNoiseError("auth", noiseNetworkClass(err), false, err)
	}
	return resp, raw, nil
}

// noiseVMAddress returns the value for hatch/token's vmAddress. The session's
// endpoint_url is authoritative when it is a wss URL; otherwise use the
// documented per-VM hostname shape.
func noiseVMAddress(sess *sessionResponse) string {
	if sess != nil {
		raw := strings.TrimSpace(sess.EndpointURL)
		if u, err := url.Parse(raw); err == nil && u.Scheme == "wss" && u.Host != "" &&
			u.User == nil && u.Fragment == "" {
			return raw
		}
		if sess.VMID != "" {
			return "wss://" + sess.VMID + ".metaaivm.com/"
		}
	}
	return ""
}

// verifyNoiseRPC runs chat.unread_count and falls back to identity only on
// 404/405. A 5xx/timeout is retryable with a fresh dial; a 401/403 or a
// double 404/405 is terminal. A 2xx body must be empty or valid JSON.
func verifyNoiseRPC(ctx context.Context, conn *noiseConn) error {
	rpcCtx, cancel := context.WithTimeout(ctx, noiseRPCTimeout)
	defer cancel()

	unread, err := conn.doRPC(rpcCtx, 1, "/chat/unread-count")
	if err != nil {
		return err
	}
	if unread.Status >= 200 && unread.Status < 300 {
		if responseBodyIsJSON(unread.Body) {
			return nil
		}
		return newNoiseError("rpc", "unread non_json", false, nil)
	}
	if unread.Status != http.StatusNotFound && unread.Status != http.StatusMethodNotAllowed {
		return newNoiseError("rpc", "unread "+noiseStatusClass(unread.Status),
			unread.Status >= 500 || unread.Status == http.StatusTooManyRequests, nil)
	}

	identity, err := conn.doRPC(rpcCtx, 2, "/identity")
	if err != nil {
		// Keep the unread status visible if the fallback itself times out.
		return &noiseError{
			Stage:     "rpc",
			Class:     fmt.Sprintf("unread %d, identity %s", unread.Status, noiseAsError(err).Class),
			Retryable: noiseAsError(err).Retryable,
			Cause:     err,
		}
	}
	if identity.Status >= 200 && identity.Status < 300 {
		if responseBodyIsJSON(identity.Body) {
			return nil
		}
		return newNoiseError("rpc", "identity non_json", false, nil)
	}
	return newNoiseError("rpc", fmt.Sprintf("unread %d, identity %d", unread.Status, identity.Status),
		identity.Status >= 500 || identity.Status == http.StatusTooManyRequests, nil)
}

func responseBodyIsJSON(body []byte) bool {
	body = bytes.TrimSpace(body)
	return len(body) == 0 || json.Valid(body)
}

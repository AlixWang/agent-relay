package muse

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// sessionResponse is the subset of GET /api/session we rely on. muse2api's
// probes pin these three fields; everything else is ignored.
type sessionResponse struct {
	Status      string `json:"status"`
	VMID        string `json:"vm_id"`
	VMState     string `json:"vm_state"`
	EndpointURL string `json:"endpoint_url"`
}

// MuseSessionUnavailable reports an HTTP 200 /api/session response whose
// workspace is not assigned. Muse can rotate hatch_sess/hatch_gw in that same
// response, so the merged jar travels with the error: the keepalive caller must
// persist it even though this attempt is a failure. Reporting the failure
// without the rotated cookies would make the next request reuse a dead session.
type MuseSessionUnavailable struct {
	Status     string
	Cookies    map[string]string
	CookiesExp map[string]int64
	Rotated    bool
}

func (e *MuseSessionUnavailable) Error() string {
	return fmt.Sprintf("muse session status %q (not assigned)", e.Status)
}

// MuseHTTPError is a non-200 response from muse.ai. Only 401 is terminal; the
// rest are transient (403/429/5xx, network and non-JSON failures) and must not
// erase the last confirmed auth/VM state. A 429 carries the Retry-After delay
// so the next keepalive can skip this account instead of hammering upstream.
type MuseHTTPError struct {
	Op         string // "session" or "wake"
	StatusCode int
	RetryAfter time.Duration
	Hint       string
}

func (e *MuseHTTPError) Error() string {
	if e.Hint == "" {
		return fmt.Sprintf("muse %s HTTP %d", e.Op, e.StatusCode)
	}
	return fmt.Sprintf("muse %s HTTP %d: %s", e.Op, e.StatusCode, e.Hint)
}

func newMuseHTTPError(op string, resp *http.Response, now time.Time) *MuseHTTPError {
	e := &MuseHTTPError{Op: op, StatusCode: resp.StatusCode}
	switch resp.StatusCode {
	case http.StatusForbidden:
		// Keep the reference implementation's wording: a 403 is an egress or
		// region restriction, not evidence that the cookie is invalid.
		e.Hint = "访问被拒绝，请检查服务器出口/地区/访问限制；不能据此判定 Cookie 失效"
	case http.StatusTooManyRequests:
		e.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), now)
		e.Hint = "请求被限流，请稍后重试"
	default:
		e.Hint = "上游请求未成功，请稍后重试并检查服务器网络"
	}
	return e
}

// parseRetryAfter accepts both delta-seconds and HTTP-date forms. A missing or
// invalid value yields zero, in which case renew applies a conservative
// one-minute default.
func parseRetryAfter(raw string, now time.Time) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		if secs > 0 {
			return time.Duration(secs) * time.Second
		}
		return 0
	}
	if at, err := http.ParseTime(raw); err == nil {
		if d := at.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// renewResult is the outcome of one session (and optional wake) attempt. It is
// always returned with the latest cookie/expiry state so the caller can
// persist even a transient failure without losing rotated cookies.
type renewResult struct {
	OK             bool
	AuthState      string // "ok" | "needs_reauth" | "" (keep prior)
	SessionStatus  string
	VMID           string
	VMState        string
	EndpointURL    string
	Cookies        map[string]string
	Exp            map[string]int64
	CookiesEnc     string
	CookiesExpJSON string
	WakeRequested  bool
	WakeOK         bool
	// Error is the session/auth/HTTP error for this attempt. Wake failures are
	// kept in WakeError so they never mark an otherwise healthy account as
	// broken.
	Error     string
	Transient bool  // session failure is upstream/transient; keep the confirmed state
	RetryAt   int64 // 429 only: unix time before which the next keepalive must skip
	Rotated   bool  // a 200 non-assigned response rotated the cookie jar
	WakeError string
	// WakeState is "unconfirmed" when a wake was attempted but Noise could not
	// verify it, and "noise_confirmed" once the handshake plus one read-only
	// RPC succeeded. "running" is legacy (vm_state=RUNNING) and is still read
	// for rollback compatibility, never written. Empty means no wake attempt.
	WakeState string
	VMStateAt int64
	At        int64
}

// renew calls /api/session, optionally wakes the VM and verifies it. The
// sequence mirrors muse2api: always refresh the session first (the vm_id may
// have changed after a sandbox restart), wake, then re-read the session and
// prove liveness with a Noise handshake plus one read-only RPC. vm_state is
// recorded for display only; a wake HTTP 200 alone is not success.
func (s *Service) renew(ctx context.Context, cookies map[string]string, exp map[string]int64, wake bool) (*renewResult, error) {
	now := s.now().Unix()
	res := &renewResult{
		Cookies: cloneCookies(cookies),
		Exp:     cloneExp(exp),
		At:      now,
	}
	before := cloneCookies(res.Cookies)
	sess, setCookies, err := s.getSession(ctx, res.Cookies)
	if len(setCookies) > 0 {
		mergeSetCookies(res.Cookies, res.Exp, setCookies, now)
	}
	if err != nil {
		applyUnavailableResult(res, err, before, setCookies)
		if errors.Is(err, ErrUnauthorized) {
			res.AuthState = "needs_reauth"
		} else {
			var unavailable *MuseSessionUnavailable
			// A 200 non-assigned response is a confirmed session failure, not
			// a transient upstream problem: record it. Every other non-401
			// failure (403/429/5xx, timeout, non-JSON) is transient and must
			// preserve the last confirmed state.
			res.Transient = !errors.As(err, &unavailable)
			if unavailable != nil {
				res.SessionStatus = unavailable.Status
			}
			res.RetryAt = retryAtFromError(err, now)
		}
		res.Error = err.Error()
		if sealErr := s.seal(res); sealErr != nil {
			return nil, sealErr
		}
		return res, err
	}
	res.SessionStatus = sess.Status
	res.VMID = sess.VMID
	res.VMState = normalizeVMState(sess.VMState)
	res.EndpointURL = sess.EndpointURL
	res.VMStateAt = now
	res.OK = true
	res.AuthState = "ok"

	if wake {
		switch {
		case sess.VMID == "":
			res.WakeError = "muse session has no vm_id; wake skipped"
			res.WakeState = "unconfirmed"
		case res.VMState == "DISABLED":
			res.WakeError = "muse VM is DISABLED; wake skipped"
			res.WakeState = "unconfirmed"
		default:
			wakeCookies, werr := s.postWake(ctx, res.Cookies, sess.VMID)
			if werr != nil {
				if at := retryAtFromError(werr, now); at > 0 {
					res.RetryAt = at
				}
				res.WakeError = werr.Error()
				res.WakeState = "unconfirmed"
			} else {
				// A 200 wake may rotate cookies. Apply them before the
				// verification GET, otherwise that request uses the old jar.
				mergeSetCookies(res.Cookies, res.Exp, wakeCookies, now)
				res.WakeRequested = true
				beforeVerify := cloneCookies(res.Cookies)
				verify, verifyCookies, verr := s.getSession(ctx, res.Cookies)
				if len(verifyCookies) > 0 {
					mergeSetCookies(res.Cookies, res.Exp, verifyCookies, now)
				}
				if verr != nil {
					applyUnavailableResult(res, verr, beforeVerify, verifyCookies)
					if at := retryAtFromError(verr, now); at > 0 {
						res.RetryAt = at
					}
					res.WakeError = "wake verification failed: " + verr.Error()
					res.WakeState = "unconfirmed"
				} else {
					res.SessionStatus = verify.Status
					res.VMID = verify.VMID
					res.VMState = normalizeVMState(verify.VMState)
					res.EndpointURL = verify.EndpointURL
					res.VMStateAt = now
					// vm_state is display-only: it is deliberately NOT a wake
					// shortcut, even when upstream claims RUNNING. Only a
					// completed Noise handshake plus one read-only RPC proves
					// the VM is actually serving.
					verr := s.verifyWake(ctx, res, verify)
					if verr != nil {
						if at := retryAtFromError(verr, now); at > 0 {
							res.RetryAt = at
						}
						res.WakeError = verr.Error()
						res.WakeState = "unconfirmed"
					} else {
						res.WakeOK = true
						res.WakeState = "noise_confirmed"
						res.WakeError = ""
					}
				}
			}
		}
	}
	if err := s.seal(res); err != nil {
		return nil, err
	}
	return res, nil
}

// applyUnavailableResult fills the typed error with the merged jar so a caller
// that only sees the error can still persist rotated cookies. Rotation is only
// reported for a 200 non-assigned response; normal keepalive rotation is not an
// anomaly.
func applyUnavailableResult(res *renewResult, err error, before map[string]string, set []*http.Cookie) {
	var unavailable *MuseSessionUnavailable
	if !errors.As(err, &unavailable) {
		return
	}
	unavailable.Cookies = cloneCookies(res.Cookies)
	unavailable.CookiesExp = cloneExp(res.Exp)
	unavailable.Rotated = cookieSetRotated(before, set, res.At)
	res.Rotated = res.Rotated || unavailable.Rotated
}

// retryAtFromError reads Retry-After from a 429. Missing/invalid values use
// the documented one-minute fallback so a rate-limited account is not hammered.
func retryAtFromError(err error, now int64) int64 {
	var httpErr *MuseHTTPError
	if !errors.As(err, &httpErr) || httpErr.StatusCode != http.StatusTooManyRequests {
		return 0
	}
	d := httpErr.RetryAfter
	if d <= 0 {
		d = time.Minute
	}
	return now + int64((d+time.Second-1)/time.Second)
}

func normalizeVMState(raw string) string {
	vm := strings.TrimSpace(raw)
	if vm == "" {
		return "UNKNOWN"
	}
	return vm
}

func (s *Service) seal(res *renewResult) error {
	raw, err := marshalCookies(res.Cookies)
	if err != nil {
		return err
	}
	enc, err := encryptCookies(s.key, raw)
	if err != nil {
		return fmt.Errorf("muse: encrypt cookies: %w", err)
	}
	res.CookiesEnc = enc
	res.CookiesExpJSON = marshalCookiesExp(res.Exp)
	return nil
}

func (s *Service) getSession(ctx context.Context, cookies map[string]string) (*sessionResponse, []*http.Cookie, error) {
	resp, body, err := s.do(ctx, http.MethodGet, "/api/session", cookies, nil)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, nil, ErrUnauthorized
	case resp.StatusCode != http.StatusOK:
		return nil, nil, newMuseHTTPError("session", resp, s.now())
	}
	var sess sessionResponse
	if err := json.Unmarshal(body, &sess); err != nil {
		// A 200 can still rotate the jar before returning a malformed body.
		// Keep those cookies while classifying the attempt as transient.
		return nil, resp.Cookies(), errors.New("muse session returned non-JSON")
	}
	if sess.Status != "assigned" {
		// Muse may rotate hatch_sess/hatch_gw on this 200 response. Return
		// both the raw cookies and a typed error so renew can merge and seal
		// them before reporting the failure.
		return nil, resp.Cookies(), &MuseSessionUnavailable{Status: sess.Status}
	}
	return &sess, resp.Cookies(), nil
}

func (s *Service) postWake(ctx context.Context, cookies map[string]string, vmID string) ([]*http.Cookie, error) {
	payload := map[string]any{
		"vm_id":              vmID,
		"retry_count":        0,
		"connect_attempt_id": newUUID(),
	}
	resp, _, err := s.do(ctx, http.MethodPost, "/api/hatch/vm/wake", cookies, payload)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, newMuseHTTPError("wake", resp, s.now())
	}
	return resp.Cookies(), nil
}

// do issues one same-origin request with the cookie jar and a 1 MiB response
// cap. Redirects are never followed (see New), so cookies cannot leak to a
// different origin.
func (s *Service) do(ctx context.Context, method, path string, cookies map[string]string, body any) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.baseURL+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", s.baseURL)
	req.Header.Set("Referer", s.baseURL+"/thread/new")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	if len(cookies) > 0 {
		req.Header.Set("Cookie", cookieHeader(cookies))
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("muse request failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, nil, fmt.Errorf("muse response read failed: %w", err)
	}
	return resp, raw, nil
}

// newUUID returns a RFC 4122 v4 UUID for connect_attempt_id. The repo already
// depends on google/uuid only transitively; generating 16 random bytes keeps
// the direct dependency surface unchanged.
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("fallback-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func cloneCookies(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneExp(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

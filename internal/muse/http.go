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
	"time"
)

// sessionResponse is the subset of GET /api/session we rely on. muse2api's
// probes pin these three fields; everything else is ignored.
type sessionResponse struct {
	Status  string `json:"status"`
	VMID    string `json:"vm_id"`
	VMState string `json:"vm_state"`
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
	Cookies        map[string]string
	Exp            map[string]int64
	CookiesEnc     string
	CookiesExpJSON string
	WakeRequested  bool
	WakeOK         bool
	Error          string
	At             int64
}

// renew calls /api/session, optionally wakes the VM and verifies it. The
// sequence mirrors muse2api: always refresh the session first (the vm_id may
// have changed after a sandbox restart), wake, then re-read the session to
// prove vm_state == RUNNING. A wake HTTP 200 alone is not success.
func (s *Service) renew(ctx context.Context, cookies map[string]string, exp map[string]int64, wake bool) (*renewResult, error) {
	now := s.now().Unix()
	res := &renewResult{
		Cookies: cloneCookies(cookies),
		Exp:     cloneExp(exp),
		At:      now,
	}
	sess, setCookies, err := s.getSession(ctx, res.Cookies)
	if err != nil {
		if errors.Is(err, ErrUnauthorized) {
			res.AuthState = "needs_reauth"
		}
		res.Error = err.Error()
		if sealErr := s.seal(res); sealErr != nil {
			return nil, sealErr
		}
		return res, err
	}
	mergeSetCookies(res.Cookies, res.Exp, setCookies, now)
	res.SessionStatus = sess.Status
	res.VMID = sess.VMID
	res.VMState = sess.VMState
	res.OK = true
	res.AuthState = "ok"

	if wake {
		switch {
		case sess.VMID == "":
			res.Error = "muse session has no vm_id; wake skipped"
		case sess.VMState == "DISABLED":
			res.Error = "muse VM is DISABLED; wake skipped"
		default:
			if err := s.postWake(ctx, res.Cookies, sess.VMID); err != nil {
				res.Error = err.Error()
			} else {
				res.WakeRequested = true
				verify, verifyCookies, verr := s.getSession(ctx, res.Cookies)
				if verr != nil {
					res.Error = "wake verification failed: " + verr.Error()
				} else {
					mergeSetCookies(res.Cookies, res.Exp, verifyCookies, now)
					res.SessionStatus = verify.Status
					res.VMID = verify.VMID
					res.VMState = verify.VMState
					if verify.Status == "assigned" && verify.VMState == "RUNNING" {
						res.WakeOK = true
					} else {
						res.Error = fmt.Sprintf("wake not confirmed (vm_state=%s)", verify.VMState)
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
		return nil, nil, fmt.Errorf("muse session HTTP %d", resp.StatusCode)
	}
	var sess sessionResponse
	if err := json.Unmarshal(body, &sess); err != nil {
		return nil, nil, errors.New("muse session returned non-JSON")
	}
	if sess.Status != "assigned" {
		return nil, nil, fmt.Errorf("muse session status %q (not assigned)", sess.Status)
	}
	return &sess, resp.Cookies(), nil
}

func (s *Service) postWake(ctx context.Context, cookies map[string]string, vmID string) error {
	payload := map[string]any{
		"vm_id":              vmID,
		"retry_count":        0,
		"connect_attempt_id": newUUID(),
	}
	resp, _, err := s.do(ctx, http.MethodPost, "/api/hatch/vm/wake", cookies, payload)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("muse wake HTTP %d", resp.StatusCode)
	}
	return nil
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

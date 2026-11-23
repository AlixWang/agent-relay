package muse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

type noiseHTTPFixture struct {
	mu sync.Mutex

	authCalls    int
	tokenCalls   int
	sessionCalls int

	authCode    int
	tokenCode   int
	sessionCode int

	authSet    []string
	tokenSet   []string
	sessionSet []string

	authFailures  int // number of initial auth/check 401s
	tokenFailures int // number of initial hatch/token 401s

	accessToken string
	hatchToken  string
	vmAddress   string
	lastAuth    string
	lastCookie  string
}

func newNoiseHTTPFixture(t *testing.T) (*noiseHTTPFixture, *httptest.Server) {
	t.Helper()
	f := &noiseHTTPFixture{
		accessToken: "access-SECRET-token",
		hatchToken:  "hatch-SECRET-token",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessionCalls++
		code := f.sessionCode
		set := append([]string(nil), f.sessionSet...)
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		for _, c := range set {
			w.Header().Add("Set-Cookie", c)
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":       "assigned",
			"vm_id":        "vm-1",
			"vm_state":     "UNKNOWN",
			"endpoint_url": "wss://vm-1.metaaivm.com/",
		})
	})
	mux.HandleFunc("/api/auth/check", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.authCalls++
		f.lastAuth = r.Header.Get("Authorization")
		code := f.authCode
		if f.authCalls <= f.authFailures {
			code = http.StatusUnauthorized
		}
		set := append([]string(nil), f.authSet...)
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		for _, c := range set {
			w.Header().Add("Set-Cookie", c)
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": f.accessToken})
	})
	mux.HandleFunc("/api/hatch/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.tokenCalls++
		code := f.tokenCode
		if f.tokenCalls <= f.tokenFailures {
			code = http.StatusUnauthorized
		}
		set := append([]string(nil), f.tokenSet...)
		f.vmAddress = body["vmAddress"]
		f.lastAuth = r.Header.Get("Authorization")
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		for _, c := range set {
			w.Header().Add("Set-Cookie", c)
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": f.hatchToken})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *noiseHTTPFixture) counts() (auth, token, session int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCalls, f.tokenCalls, f.sessionCalls
}

func newNoiseService(baseURL, gatewayURL string) *Service {
	return &Service{
		baseURL:    baseURL,
		gatewayURL: gatewayURL,
		client:     &http.Client{Timeout: 2 * time.Second},
		now:        time.Now,
	}
}

func newNoiseResult() *renewResult {
	return &renewResult{
		Cookies: map[string]string{"hatch_sess": "sess-SECRET"},
		Exp:     map[string]int64{"hatch_sess": time.Now().Add(time.Hour).Unix()},
	}
}

func TestVerifyNoiseHonorsCancellation(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := svc.verifyNoise(ctx, newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil {
		t.Fatal("expected cancellation failure")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("cancellation was not honored promptly: %s", elapsed)
	}
	if !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("error should say canceled: %v", err)
	}
	if ws.Dials() != 0 {
		t.Fatalf("cancelled verify dialed %d times", ws.Dials())
	}
}

func TestVerifyNoiseSuccess(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	var got musetest.Request
	ws.SetHandler(func(r musetest.Request) musetest.Response {
		got = r
		return musetest.Response{Status: 200, Body: []byte(`{"unread_count":0}`), EndBody: true}
	})
	svc := newNoiseService(httpSrv.URL, ws.URL)
	res := newNoiseResult()
	if err := svc.verifyNoise(context.Background(), res, &sessionResponse{
		Status: "assigned", VMID: "vm-1", EndpointURL: "wss://vm-1.metaaivm.com/",
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	auth, token, session := httpFake.counts()
	if auth != 1 || token != 1 || session != 0 {
		t.Fatalf("token calls: auth=%d token=%d session=%d", auth, token, session)
	}
	if httpFake.vmAddress != "wss://vm-1.metaaivm.com/" {
		t.Fatalf("vmAddress: %q", httpFake.vmAddress)
	}
	if !strings.Contains(httpFake.lastCookie, "hatch_sess=sess-SECRET") {
		t.Fatalf("cookie header: %q", httpFake.lastCookie)
	}
	if httpFake.lastAuth != "Bearer access-SECRET-token" {
		t.Fatalf("authorization header: %q", httpFake.lastAuth)
	}
	if got.Verb != "GET" || got.Path != "/chat/unread-count" || got.StreamID != 1 {
		t.Fatalf("request: %+v", got)
	}
	if got.Headers["x-request-id"] == "" {
		t.Fatalf("x-request-id missing: %+v", got.Headers)
	}
	if len(got.Body) != 0 {
		t.Fatalf("GET body should be empty: %q", got.Body)
	}
}

func TestVerifyNoiseEndpointFallbackUsesVMID(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	res := newNoiseResult()
	if err := svc.verifyNoise(context.Background(), res, &sessionResponse{
		Status: "assigned", VMID: "vm-9", EndpointURL: "https://wrong.example/",
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if httpFake.vmAddress != "wss://vm-9.metaaivm.com/" {
		t.Fatalf("fallback vmAddress: %q", httpFake.vmAddress)
	}
}

func TestVerifyNoise404FallsBackToIdentity(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	ws.SetHandler(func(r musetest.Request) musetest.Response {
		if r.Path == "/chat/unread-count" {
			return musetest.Response{Status: 404, EndBody: true}
		}
		if r.Path == "/identity" {
			return musetest.Response{Status: 200, Body: []byte(`{"id":"muse-bot"}`), EndBody: true}
		}
		return musetest.Response{Status: 500, EndBody: true}
	})
	svc := newNoiseService(httpSrv.URL, ws.URL)
	if err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	reqs := ws.Requests()
	if len(reqs) != 2 || reqs[0].Path != "/chat/unread-count" || reqs[1].Path != "/identity" ||
		reqs[1].StreamID != 2 {
		t.Fatalf("fallback requests: %+v", reqs)
	}
}

func TestVerifyNoise500DoesNotFallback(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	ws.SetHandler(func(r musetest.Request) musetest.Response {
		return musetest.Response{Status: 500, Body: []byte("boom"), EndBody: true}
	})
	svc := newNoiseService(httpSrv.URL, ws.URL)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "unread 500") {
		t.Fatalf("error: %v", err)
	}
	reqs := ws.Requests()
	if len(reqs) == 0 {
		t.Fatal("no requests reached the mock")
	}
	for _, req := range reqs {
		if req.Path != "/chat/unread-count" {
			t.Fatalf("500 should not fallback: %+v", reqs)
		}
	}
	if ws.Dials() != noiseMaxAttempts {
		t.Fatalf("5xx should use the dial budget: dials=%d", ws.Dials())
	}
}

func TestVerifyNoiseNonJSONFails(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	ws.SetHandler(func(r musetest.Request) musetest.Response {
		return musetest.Response{Status: 200, Body: []byte("not json"), EndBody: true}
	})
	svc := newNoiseService(httpSrv.URL, ws.URL)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "non_json") {
		t.Fatalf("error: %v", err)
	}
}

func TestVerifyNoiseToken401RefreshesOnce(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	httpFake.authFailures = 1
	httpFake.sessionSet = []string{"hatch_sess=refreshed-SECRET; Path=/; Max-Age=3600"}
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	res := newNoiseResult()
	if err := svc.verifyNoise(context.Background(), res, &sessionResponse{Status: "assigned", VMID: "vm-1"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	auth, token, session := httpFake.counts()
	if auth != 2 || token != 1 || session != 1 {
		t.Fatalf("refresh calls: auth=%d token=%d session=%d", auth, token, session)
	}
	if res.Cookies["hatch_sess"] != "refreshed-SECRET" {
		t.Fatalf("rotated cookie not merged: %+v", res.Cookies)
	}
	if !strings.Contains(httpFake.lastCookie, "hatch_sess=refreshed-SECRET") {
		t.Fatalf("retry did not use refreshed cookie: %q", httpFake.lastCookie)
	}
}

func TestVerifyNoiseToken401TwiceStops(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	httpFake.authCode = http.StatusUnauthorized
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "noise auth: 401") {
		t.Fatalf("error: %v", err)
	}
	auth, token, session := httpFake.counts()
	if auth != 2 || token != 0 || session != 1 {
		t.Fatalf("must stop after one refresh: auth=%d token=%d session=%d", auth, token, session)
	}
}

func TestVerifyNoiseToken429CarriesRetryAfter(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/check" {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(ts.Close)
	ws := musetest.New(t)
	svc := newNoiseService(ts.URL, ws.URL)
	now := time.Now().Unix()
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("error: %v", err)
	}
	if at := retryAtFromError(err, now); at < now+119 {
		t.Fatalf("Retry-After not preserved: at=%d now=%d err=%v", at, now, err)
	}
}

func TestVerifyNoiseThreeDialBudgetAndTokenOnce(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	ws.FailFirstDials(2)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	start := time.Now()
	if err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("retry budget took too long: %s", elapsed)
	}
	if ws.Dials() != 3 {
		t.Fatalf("dials: %d", ws.Dials())
	}
	auth, token, session := httpFake.counts()
	if auth != 1 || token != 1 || session != 0 {
		t.Fatalf("token must be fetched once: auth=%d token=%d session=%d", auth, token, session)
	}
}

func TestVerifyNoiseDialFailuresRedactSecrets(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	// A closed loopback port fails fast; three attempts still run.
	dead := httptest.NewServer(http.NotFoundHandler())
	gateway := "ws" + strings.TrimPrefix(dead.URL, "http") + "/v1/noise"
	dead.Close()
	svc := newNoiseService(httpSrv.URL, gateway)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	for _, bad := range []string{"SECRET", "access-SECRET", "hatch-SECRET", "auth_token", "wss://"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("error leaked %q: %s", bad, msg)
		}
	}
	if !strings.Contains(msg, "attempt 3/3") {
		t.Fatalf("expected three attempts: %s", msg)
	}
	var ne *noiseError
	if !errors.As(err, &ne) || ne.Stage != "dial" {
		t.Fatalf("typed error: %#v", err)
	}
}

func TestVerifyNoiseTokenRotationMergedOnFailure(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	httpFake.tokenCode = http.StatusInternalServerError
	httpFake.tokenSet = []string{"hatch_gw=rotated-gw; Path=/; Max-Age=3600"}
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	res := newNoiseResult()
	err := svc.verifyNoise(context.Background(), res, &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "noise token: 500") {
		t.Fatalf("error: %v", err)
	}
	if res.Cookies["hatch_gw"] != "rotated-gw" {
		t.Fatalf("Set-Cookie on a failed token response was dropped: %+v", res.Cookies)
	}
}

func TestVerifyNoiseResetFailsWithoutRetry(t *testing.T) {
	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	ws.SetHandler(func(r musetest.Request) musetest.Response {
		return musetest.Response{Reset: &musetest.Reset{Code: 4, Reason: "boom"}}
	})
	svc := newNoiseService(httpSrv.URL, ws.URL)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "reset 4") {
		t.Fatalf("error: %v", err)
	}
	if got := len(ws.Requests()); got != 1 {
		t.Fatalf("reset should not be retried on the same stream, requests=%d", got)
	}
	if ws.Dials() != 1 {
		t.Fatalf("non-retryable reset redialed: %d", ws.Dials())
	}
}

func TestVerifyNoiseSessionRefreshFailureDoesNotMarkAuth(t *testing.T) {
	httpFake, httpSrv := newNoiseHTTPFixture(t)
	httpFake.authCode = http.StatusUnauthorized
	httpFake.sessionCode = http.StatusUnauthorized
	ws := musetest.New(t)
	svc := newNoiseService(httpSrv.URL, ws.URL)
	res := newNoiseResult()
	res.AuthState = "ok"
	err := svc.verifyNoise(context.Background(), res, &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil {
		t.Fatal("expected failure")
	}
	if res.AuthState != "ok" {
		t.Fatalf("verifyNoise changed AuthState: %+v", res)
	}
}

func TestVerifyNoiseRetriesDial502(t *testing.T) {
	origDelays := noiseDial5xxDelays
	noiseDial5xxDelays = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	t.Cleanup(func() { noiseDial5xxDelays = origDelays })

	_, httpSrv := newNoiseHTTPFixture(t)
	ws := musetest.New(t)
	var hits int
	var mu sync.Mutex
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		n := hits
		mu.Unlock()
		if n <= 2 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		// Redirect/upgrade not needed if we fail on 3rd? Even better: test that
		// 2x 502 triggers 3rd dial (which hits 403 -> non-retryable and stops
		// immediately), proving 502 IS retried while 403 is NOT retried.
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(proxy.Close)

	gateway := "ws" + strings.TrimPrefix(proxy.URL, "http") + "/v1/noise"
	svc := newNoiseService(httpSrv.URL, gateway)
	err := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err == nil || !strings.Contains(err.Error(), "noise dial: 403 (attempt 3/3") {
		t.Fatalf("expected 502x2 then 403 on attempt 3/3, got: %v", err)
	}
	if hits != 3 {
		t.Fatalf("dial attempts = %d, want 3", hits)
	}

	// Single 403 must fail on attempt 1/3 without retrying.
	mu.Lock()
	hits = 10
	mu.Unlock()
	err2 := svc.verifyNoise(context.Background(), newNoiseResult(), &sessionResponse{Status: "assigned", VMID: "vm-1"})
	if err2 == nil || !strings.Contains(err2.Error(), "noise dial: 403 (attempt 1/3") {
		t.Fatalf("403 must not retry, got: %v", err2)
	}
	_ = ws
}

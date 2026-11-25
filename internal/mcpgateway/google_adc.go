package mcpgateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CredentialSource resolves a secret:<name> reference.
type CredentialSource interface {
	Resolve(ref string) (string, error)
}

const defaultGoogleTokenURL = "https://oauth2.googleapis.com/token"

// GoogleADCOptions configures a shared OAuth2 authorized_user ADC token
// provider for Google upstream services (AdSense, Google Analytics).
type GoogleADCOptions struct {
	ServiceName   string
	CredentialRef string
	Credentials   CredentialSource
	HTTPClient    *http.Client
	Timeout       time.Duration
	TokenURL      string
	Now           func() time.Time
}

// GoogleADC exchanges an authorized_user ADC secret for an OAuth2 access
// token, serializes concurrent refreshes, invalidates when the underlying
// secret changes, and avoids clobbering newer tokens on stale 401s.
type GoogleADC struct {
	serviceName string
	credRef     string
	creds       CredentialSource
	client      *http.Client
	tokenURL    string

	refreshMu sync.Mutex
	mu        sync.Mutex
	now       func() time.Time
	token     string
	adcHash   [32]byte
	expires   time.Time
}

type adcFile struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// NewGoogleADC constructs a thread-safe Google OAuth2 ADC token provider.
func NewGoogleADC(opts GoogleADCOptions) *GoogleADC {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := opts.HTTPClient
	if client == nil {
		client = NoRedirectClient(timeout)
	}
	tokenURL := opts.TokenURL
	if tokenURL == "" {
		tokenURL = defaultGoogleTokenURL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	name := opts.ServiceName
	if name == "" {
		name = "Google"
	}
	return &GoogleADC{
		serviceName: name,
		credRef:     opts.CredentialRef,
		creds:       opts.Credentials,
		client:      client,
		tokenURL:    tokenURL,
		now:         now,
	}
}

// SetClock overrides the clock (used by tests).
func (g *GoogleADC) SetClock(now func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if now != nil {
		g.now = now
	}
}

func (g *GoogleADC) credError() *Error {
	return &Error{
		Code:       CodeCredentialMissing,
		Message:    g.serviceName + " credential is not available",
		HTTPStatus: http.StatusOK,
	}
}

// Token returns a valid OAuth2 access token, refreshing when expired or when
// the resolved ADC JSON changed (secret rotation).
func (g *GoogleADC) Token(ctx context.Context) (string, *Error) {
	if g.creds == nil || g.credRef == "" {
		return "", g.credError()
	}
	raw, err := g.creds.Resolve(g.credRef)
	if err != nil {
		return "", g.credError()
	}
	sum := sha256.Sum256([]byte(raw))

	g.mu.Lock()
	if g.token != "" && g.adcHash == sum && g.now().Before(g.expires) {
		tok := g.token
		g.mu.Unlock()
		return tok, nil
	}
	g.mu.Unlock()

	g.refreshMu.Lock()
	defer g.refreshMu.Unlock()

	// Re-resolve and re-check under refreshMu so concurrent callers reuse the
	// token fetched by the first waiter.
	raw, err = g.creds.Resolve(g.credRef)
	if err != nil {
		return "", g.credError()
	}
	sum = sha256.Sum256([]byte(raw))

	g.mu.Lock()
	if g.token != "" && g.adcHash == sum && g.now().Before(g.expires) {
		tok := g.token
		g.mu.Unlock()
		return tok, nil
	}
	g.mu.Unlock()

	var adc adcFile
	if err := json.Unmarshal([]byte(raw), &adc); err != nil || adc.Type != "authorized_user" ||
		adc.ClientID == "" || adc.ClientSecret == "" || adc.RefreshToken == "" {
		return "", g.credError()
	}
	form := url.Values{
		"client_id":     {adc.ClientID},
		"client_secret": {adc.ClientSecret},
		"refresh_token": {adc.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", UpstreamFailure(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "relay-mcp/1.0")
	resp, err := g.client.Do(req)
	if err != nil {
		if IsTimeout(err) {
			return "", UpstreamFailure(ErrUpstreamTimeout)
		}
		return "", UpstreamFailure(ErrUpstreamNetwork)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		if IsTimeout(err) || IsTimeout(ctx.Err()) {
			return "", UpstreamFailure(ErrUpstreamTimeout)
		}
		return "", UpstreamFailure(ErrUpstreamNetwork)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &Error{
			Code:       CodeUpstreamError,
			HTTPStatus: http.StatusOK,
			Message:    fmt.Sprintf("%s token refresh failed (HTTP %d); operator must re-authorize the ADC", g.serviceName, resp.StatusCode),
			Details:    map[string]any{"upstream_status": resp.StatusCode},
		}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "", &Error{
			Code:       CodeUpstreamError,
			Message:    g.serviceName + " token refresh returned no access token",
			HTTPStatus: http.StatusOK,
		}
	}
	ttl := tok.ExpiresIn
	if ttl <= 0 {
		ttl = 3000
	}
	g.mu.Lock()
	g.token = tok.AccessToken
	g.adcHash = sum
	g.expires = g.now().Add(time.Duration(ttl-60) * time.Second)
	g.mu.Unlock()
	return tok.AccessToken, nil
}

// Invalidate drops the cached token if it still matches badToken (or always if
// badToken is empty), preventing a stale 401 from wiping a newer token.
func (g *GoogleADC) Invalidate(badToken string) {
	g.mu.Lock()
	if badToken == "" || g.token == badToken {
		g.token = ""
		g.expires = time.Time{}
	}
	g.mu.Unlock()
}

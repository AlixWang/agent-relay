// Package muse owns optional muse.ai account authorization, session renewal
// and cloud-VM wake-ups (DESIGN §4.9).
//
// The control plane is deliberately tiny: GET /api/session refreshes the
// hatch_sess/hatch_vml cookies and reports the current vm_id/vm_state, and
// POST /api/hatch/vm/wake asks Muse to boot the sandbox. Waking is never
// inferred from the wake HTTP 200: a second /api/session must report RUNNING.
package muse

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

var (
	ErrUnavailable       = errors.New("muse integration unavailable")
	ErrUnknownPeer       = errors.New("unknown peer")
	ErrNotFound          = errors.New("unknown muse account")
	ErrAlreadyAuthorized = errors.New("peer already has a muse account")
	ErrMissingCookies    = errors.New("missing essential cookies")
	ErrBadCookies        = errors.New("invalid muse cookie")
	ErrUnauthorized      = errors.New("muse session unauthorized")
	ErrBusy              = errors.New("muse keepalive already running")
)

// Options configures the optional Muse integration. Zero durations select the
// documented defaults; an empty BaseURL means https://muse.ai.
type Options struct {
	BaseURL           string
	ProxyURL          string
	HTTPTimeout       time.Duration
	KeepaliveInterval time.Duration
	WakeCooldown      time.Duration
	DataDir           string
	AutoWake          bool
	Now               func() time.Time
}

// Account is the write-only-safe view of one authorization. It intentionally
// has no cookie values or ciphertext: only names, essential status and expiry.
type Account struct {
	ID            string `json:"id"`
	PeerID        string `json:"peer_id"`
	Label         string `json:"label"`
	AuthState     string `json:"auth_state"`
	SessionStatus string `json:"session_status"`
	VMID          string `json:"vm_id,omitempty"`
	VMState       string `json:"vm_state,omitempty"`
	VMStateAt     int64  `json:"vm_state_at"`

	ExpiresAt   int64            `json:"expires_at"`
	CookiesExp  map[string]int64 `json:"cookies_exp"`
	CookieNames []string         `json:"cookie_names"`
	EssentialOK bool             `json:"essential_ok"`

	LastKeepaliveAt int64  `json:"last_keepalive_at"`
	LastAttemptAt   int64  `json:"last_attempt_at"`
	LastAttemptOK   bool   `json:"last_attempt_ok"`
	LastError       string `json:"last_error,omitempty"`
	LastWakeAt      int64  `json:"last_wake_at"`
	LastWakeOK      bool   `json:"last_wake_ok"`
	// LastWakeError/LastWakeState describe only the wake step. They must not
	// leak into LastError, which stays session/auth-scoped.
	LastWakeError string `json:"last_wake_error,omitempty"`
	LastWakeState string `json:"last_wake_state,omitempty"`
	AutoWake      bool   `json:"auto_wake"`
	Enabled       bool   `json:"enabled"`
	AuthorizedAt  int64  `json:"authorized_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

// Result is one keepalive/wake attempt. OK means the Muse session was valid;
// WakeOK separately proves the VM reached RUNNING. Error is session/auth/HTTP
// scoped; WakeError is wake scoped, so a failed wake does not turn a healthy
// session result into a general failure.
type Result struct {
	ID            string `json:"id"`
	PeerID        string `json:"peer_id"`
	Label         string `json:"label"`
	OK            bool   `json:"ok"`
	AuthState     string `json:"auth_state"`
	SessionStatus string `json:"session_status"`
	VMID          string `json:"vm_id,omitempty"`
	VMState       string `json:"vm_state,omitempty"`
	VMStateAt     int64  `json:"vm_state_at"`
	WakeRequested bool   `json:"wake_requested"`
	WakeOK        bool   `json:"wake_ok"`
	ExpiresAt     int64  `json:"expires_at"`
	Error         string `json:"error,omitempty"`
	WakeError     string `json:"wake_error,omitempty"`
	WakeState     string `json:"wake_state,omitempty"`
	At            int64  `json:"at"`
}

// Summary is the outcome of one KeepaliveAll run.
type Summary struct {
	StartedAt       int64     `json:"started_at"`
	FinishedAt      int64     `json:"finished_at"`
	Total           int       `json:"total"`
	OK              int       `json:"ok"`
	Failed          int       `json:"failed"`
	NeedsReauth     int       `json:"needs_reauth"`
	WakeOK          int       `json:"wake_ok"`
	Skipped         int       `json:"skipped"`
	Results         []*Result `json:"results"`
	SkippedAccounts []string  `json:"skipped_accounts,omitempty"`
}

// Status is the console's view of the background loop.
type Status struct {
	Running     bool     `json:"running"`
	LoopEnabled bool     `json:"loop_enabled"`
	LastRun     int64    `json:"last_run"`
	NextRun     int64    `json:"next_run"`
	LastResult  *Summary `json:"last_result,omitempty"`
}

// Service serializes writes through opMu (SaveMuseAccount writes every column,
// so concurrent keepalive/settings updates must not interleave) and prevents
// two full keepalive runs through runMu.
type Service struct {
	st      store.Store
	opts    Options
	key     []byte
	baseURL string
	client  *http.Client
	now     func() time.Time

	mu         sync.Mutex
	runMu      sync.Mutex
	opMu       sync.Mutex
	running    bool
	lastRun    int64
	nextRun    int64
	lastResult *Summary
	waking     map[string]bool
	// backoff is keyed by account ID and holds the 429 Retry-After deadline.
	// It is deliberately in-memory: a restart may retry once early, but no
	// schema migration is needed for a transient rate-limit signal.
	backoff map[string]int64
}

// New validates the endpoint/proxy, loads the cookie key and builds the HTTP
// client. A failure disables the optional feature; the relay itself keeps
// running (main only logs the error).
func New(st store.Store, opt Options) (*Service, error) {
	if st == nil {
		return nil, errors.New("muse: store required")
	}
	base := strings.TrimRight(strings.TrimSpace(opt.BaseURL), "/")
	if base == "" {
		base = "https://muse.ai"
	}
	if err := validateBaseURL(base); err != nil {
		return nil, err
	}
	timeout := opt.HTTPTimeout
	if timeout <= 0 {
		timeout = 12 * time.Second
	}
	cooldown := opt.WakeCooldown
	if cooldown <= 0 {
		cooldown = 5 * time.Minute
	}
	if opt.KeepaliveInterval < 0 {
		opt.KeepaliveInterval = 0
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(opt.ProxyURL) != "" {
		pu, err := url.Parse(opt.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("muse: bad proxy URL: %w", err)
		}
		if err := validateProxyURL(pu); err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	key, created, err := loadOrCreateKey(opt.DataDir)
	if err != nil {
		return nil, err
	}
	now := opt.Now
	if now == nil {
		now = time.Now
	}
	opt.BaseURL = base
	opt.HTTPTimeout = timeout
	opt.WakeCooldown = cooldown
	s := &Service{
		st:      st,
		opts:    opt,
		key:     key,
		baseURL: base,
		now:     now,
		client: &http.Client{
			Timeout:   timeout,
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		waking:  map[string]bool{},
		backoff: map[string]int64{},
	}
	if created {
		if accounts, err := st.ListMuseAccounts(); err == nil && len(accounts) > 0 {
			log.Printf("WARN: muse: created a new cookie key; %d existing authorization(s) must be re-entered", len(accounts))
		}
	}
	return s, nil
}

// Authorize validates a pasted cookie jar, performs one session renewal + VM
// wake, and stores the encrypted jar. A 401 is rejected without inserting a
// row; transient failures are stored as unverified so a later keepalive can
// recover without making the operator paste the cookie again.
func (s *Service) Authorize(ctx context.Context, peerID, label, cookieInput string, autoWake, enabled bool) (*Account, error) {
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return nil, ErrUnknownPeer
	}
	peer, err := s.st.GetPeer(peerID)
	if err != nil {
		return nil, err
	}
	if peer == nil {
		return nil, ErrUnknownPeer
	}
	if existing, err := s.st.GetMuseAccountByPeer(peerID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, ErrAlreadyAuthorized
	}
	cookies, err := parseCookieInput(cookieInput)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCookies, err)
	}
	if missing := missingEssential(cookies); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingCookies, strings.Join(missing, ", "))
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()
	res, rerr := s.renew(ctx, cookies, map[string]int64{}, true)
	if errors.Is(rerr, ErrUnauthorized) {
		return nil, ErrUnauthorized
	}
	if res == nil {
		return nil, rerr
	}
	now := s.now().Unix()
	a := &store.MuseAccount{
		ID:           newAccountID(),
		PeerID:       peerID,
		Label:        normalizeLabel(label, peerID),
		AutoWake:     autoWake,
		Enabled:      enabled,
		AuthState:    "unverified",
		AuthorizedAt: now,
		UpdatedAt:    now,
	}
	applyRenew(a, res, now, res.WakeRequested)
	if a.AuthState == "" {
		a.AuthState = "unverified"
	}
	if !res.Transient && a.LastError == "" && rerr != nil {
		a.LastError = truncate(rerr.Error(), 500)
	}
	if err := s.st.CreateMuseAccount(a); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrAlreadyAuthorized
		}
		return nil, err
	}
	s.recordRetry(a.ID, res)
	return s.toAccount(a), nil
}

// Reauthorize probes the new cookie jar before replacing the stored one: a 401
// leaves the previous (possibly still valid) authorization untouched.
func (s *Service) Reauthorize(ctx context.Context, id, cookieInput string) (*Account, error) {
	cookies, err := parseCookieInput(cookieInput)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadCookies, err)
	}
	if missing := missingEssential(cookies); len(missing) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrMissingCookies, strings.Join(missing, ", "))
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	res, rerr := s.renew(ctx, cookies, map[string]int64{}, true)
	if errors.Is(rerr, ErrUnauthorized) {
		return nil, ErrUnauthorized
	}
	if res == nil {
		return nil, rerr
	}
	now := s.now().Unix()
	applyRenew(a, res, now, res.WakeRequested)
	if a.AuthState == "" {
		a.AuthState = "unverified"
	}
	if !res.Transient && a.LastError == "" && rerr != nil {
		a.LastError = truncate(rerr.Error(), 500)
	}
	if res.OK {
		a.AuthorizedAt = now
	}
	if err := s.st.SaveMuseAccount(a); err != nil {
		return nil, err
	}
	s.recordRetry(a.ID, res)
	return s.toAccount(a), nil
}

// Update changes labels, the peer binding and the two permission switches.
// Re-binding requires the target peer to exist and be unbound.
func (s *Service) Update(id string, label, peerID *string, autoWake, enabled *bool) (*Account, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	if peerID != nil {
		target := strings.TrimSpace(*peerID)
		if target == "" {
			return nil, ErrUnknownPeer
		}
		if target != a.PeerID {
			p, err := s.st.GetPeer(target)
			if err != nil {
				return nil, err
			}
			if p == nil {
				return nil, ErrUnknownPeer
			}
			other, err := s.st.GetMuseAccountByPeer(target)
			if err != nil {
				return nil, err
			}
			if other != nil && other.ID != a.ID {
				return nil, ErrAlreadyAuthorized
			}
			a.PeerID = target
		}
	}
	if label != nil {
		a.Label = truncate(strings.TrimSpace(*label), 200)
	}
	if autoWake != nil {
		a.AutoWake = *autoWake
	}
	if enabled != nil {
		a.Enabled = *enabled
	}
	a.UpdatedAt = s.now().Unix()
	if err := s.st.SaveMuseAccount(a); err != nil {
		return nil, err
	}
	return s.toAccount(a), nil
}

// Revoke deletes the authorization and its ciphertext.
func (s *Service) Revoke(id string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return err
	}
	if a == nil {
		return ErrNotFound
	}
	return s.st.DeleteMuseAccount(id)
}

func (s *Service) Get(id string) (*Account, error) {
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	return s.toAccount(a), nil
}

func (s *Service) List() ([]*Account, error) {
	rows, err := s.st.ListMuseAccounts()
	if err != nil {
		return nil, err
	}
	out := make([]*Account, 0, len(rows))
	for _, a := range rows {
		out = append(out, s.toAccount(a))
	}
	return out, nil
}

// Keepalive renews one account and records the outcome. Muse-level failures
// are returned in Result (the admin API stays HTTP 200); only store/decrypt
// plumbing errors are returned as Go errors.
func (s *Service) Keepalive(ctx context.Context, id string) (*Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	now := s.now().Unix()
	cookies, exp, oerr := s.openAccount(a)
	if oerr != nil {
		a.AuthState = "needs_reauth"
		a.LastError = truncate("cookie unavailable: "+oerr.Error(), 500)
		a.LastAttemptAt = now
		a.LastAttemptOK = false
		a.UpdatedAt = now
		if err := s.st.SaveMuseAccount(a); err != nil {
			return nil, err
		}
		return &Result{ID: a.ID, PeerID: a.PeerID, Label: a.Label, OK: false,
			AuthState: a.AuthState, Error: a.LastError, At: now}, nil
	}
	res, rerr := s.renew(ctx, cookies, exp, true)
	if res == nil {
		return nil, rerr
	}
	applyRenew(a, res, now, false)
	if !res.Transient && a.LastError == "" && rerr != nil {
		a.LastError = truncate(rerr.Error(), 500)
	}
	if err := s.st.SaveMuseAccount(a); err != nil {
		return nil, err
	}
	s.recordRetry(a.ID, res)
	return toResult(a, res), nil
}

// Wake is Keepalive plus a recorded wake event (last_wake_at/last_wake_ok).
func (s *Service) Wake(ctx context.Context, id string) (*Result, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	a, err := s.st.GetMuseAccount(id)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	now := s.now().Unix()
	cookies, exp, oerr := s.openAccount(a)
	if oerr != nil {
		a.AuthState = "needs_reauth"
		a.LastError = truncate("cookie unavailable: "+oerr.Error(), 500)
		a.LastAttemptAt = now
		a.LastAttemptOK = false
		a.LastWakeAt = now
		a.LastWakeOK = false
		a.LastWakeState = "unconfirmed"
		a.LastWakeError = truncate("wake skipped: "+oerr.Error(), 500)
		a.UpdatedAt = now
		if err := s.st.SaveMuseAccount(a); err != nil {
			return nil, err
		}
		return &Result{ID: a.ID, PeerID: a.PeerID, Label: a.Label, OK: false,
			AuthState: a.AuthState, Error: a.LastError,
			WakeState: a.LastWakeState, WakeError: a.LastWakeError, At: now}, nil
	}
	res, rerr := s.renew(ctx, cookies, exp, true)
	if res == nil {
		return nil, rerr
	}
	applyRenew(a, res, now, true)
	if !res.Transient && a.LastError == "" && rerr != nil {
		a.LastError = truncate(rerr.Error(), 500)
	}
	if err := s.st.SaveMuseAccount(a); err != nil {
		return nil, err
	}
	s.recordRetry(a.ID, res)
	return toResult(a, res), nil
}

// KeepaliveAll runs the background sweep. force=true also retries accounts
// marked needs_reauth (manual "全部保活"); the background loop leaves them
// alone until the operator re-authorizes.
func (s *Service) KeepaliveAll(ctx context.Context, force bool) (*Summary, error) {
	if !s.runMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.runMu.Unlock()

	started := s.now().Unix()
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	accounts, err := s.st.ListMuseAccounts()
	if err != nil {
		return nil, err
	}
	sum := &Summary{StartedAt: started, Results: []*Result{}}
	for _, a := range accounts {
		if ctx.Err() != nil {
			break
		}
		skip := func(reason string) {
			sum.Skipped++
			sum.SkippedAccounts = append(sum.SkippedAccounts, a.PeerID)
			s.logVerdict(a, resultFromAccount(a), reason)
		}
		if !a.Enabled {
			skip("disabled")
			continue
		}
		// A 429 Retry-After window is authoritative even for a manual
		// "全部保活": retrying by hand does not make the upstream forget the
		// rate limit. It is checked before the needs_reauth gate.
		if until := s.retryAt(a.ID); until > 0 {
			skip(fmt.Sprintf("backoff_until=%d", until))
			continue
		}
		if !force && a.AuthState == "needs_reauth" {
			skip("needs_reauth")
			continue
		}
		res, kerr := s.Keepalive(ctx, a.ID)
		if kerr != nil {
			sum.Failed++
			failed := &Result{
				ID: a.ID, PeerID: a.PeerID, Label: a.Label, OK: false,
				AuthState: a.AuthState, Error: kerr.Error(), At: s.now().Unix(),
			}
			sum.Results = append(sum.Results, failed)
			s.logVerdict(a, failed, "")
		} else {
			sum.Results = append(sum.Results, res)
			if res.OK {
				sum.OK++
			} else {
				sum.Failed++
			}
			if res.AuthState == "needs_reauth" {
				sum.NeedsReauth++
			}
			if res.WakeOK {
				sum.WakeOK++
			}
			s.logVerdict(a, res, "")
		}
		select {
		case <-ctx.Done():
		case <-time.After(200 * time.Millisecond):
		}
	}
	sum.Total = len(accounts)
	sum.FinishedAt = s.now().Unix()
	s.mu.Lock()
	s.lastRun = started
	s.lastResult = sum
	if s.opts.KeepaliveInterval > 0 {
		s.nextRun = sum.FinishedAt + int64(s.opts.KeepaliveInterval/time.Second)
	}
	s.mu.Unlock()
	return sum, nil
}

func resultFromAccount(a *store.MuseAccount) *Result {
	if a == nil {
		return &Result{}
	}
	return &Result{
		ID: a.ID, PeerID: a.PeerID, Label: a.Label,
		OK: a.LastAttemptOK, AuthState: a.AuthState,
		SessionStatus: a.SessionStatus, VMID: a.VMID, VMState: a.VMState,
		VMStateAt: a.VMStateAt, WakeOK: a.LastWakeOK,
		Error: a.LastError, WakeError: a.LastWakeError, WakeState: a.LastWakeState,
		At: a.LastAttemptAt,
	}
}

// logVerdict emits one line per account per round, including skipped
// accounts. It is the operator-facing way to see which node failed and why.
// Cookie values and ciphertext are never included; errors are bounded.
func (s *Service) logVerdict(a *store.MuseAccount, r *Result, skipped string) {
	if r == nil {
		r = &Result{}
	}
	peerID, accountID := r.PeerID, r.ID
	if a != nil {
		if peerID == "" {
			peerID = a.PeerID
		}
		if accountID == "" {
			accountID = a.ID
		}
	}
	vm, vmAt := r.VMState, r.VMStateAt
	if a != nil {
		// A transient failure has no new VM observation; report the last
		// confirmed value rather than "UNKNOWN", which would look like a
		// state change the upstream never reported.
		if vm == "" {
			vm, vmAt = a.VMState, a.VMStateAt
		}
	}
	if vm == "" {
		vm = "UNKNOWN"
	}
	detail := fmt.Sprintf("peer_id=%s account=%s ok=%v auth=%s vm_state=%s vm_state_at=%d wake_requested=%v wake_ok=%v wake_state=%s error=%s wake_error=%s",
		peerID, accountID, r.OK, r.AuthState, vm, vmAt,
		r.WakeRequested, r.WakeOK, r.WakeState,
		truncate(r.Error, 160), truncate(r.WakeError, 160))
	if skipped != "" {
		detail += " skipped=" + skipped
	}
	log.Printf("muse: keepalive %s", detail)
}

func (s *Service) retryAt(accountID string) int64 {
	if accountID == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	until := s.backoff[accountID]
	if until == 0 {
		return 0
	}
	if until <= s.now().Unix() {
		delete(s.backoff, accountID)
		return 0
	}
	return until
}

func (s *Service) recordRetry(accountID string, res *renewResult) {
	if accountID == "" || res == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if res.RetryAt > 0 {
		s.backoff[accountID] = res.RetryAt
	} else if res.OK {
		delete(s.backoff, accountID)
	}
}

// MaybeWakeOffline is the presence hook. It returns immediately; the actual
// wake runs in a goroutine so a 30s presence sweep is never blocked by Muse.
func (s *Service) MaybeWakeOffline(peerID string, offlineSecs int64) {
	if s == nil || !s.opts.AutoWake || offlineSecs <= 0 {
		return
	}
	a, err := s.st.GetMuseAccountByPeer(peerID)
	if err != nil || a == nil {
		return
	}
	if !a.Enabled || !a.AutoWake || a.AuthState == "needs_reauth" || a.VMState == "DISABLED" {
		return
	}
	peer, err := s.st.GetPeer(peerID)
	if err != nil || peer == nil || peer.Status == "suspended" {
		return
	}
	now := s.now().Unix()
	cooldown := int64(s.opts.WakeCooldown / time.Second)
	if cooldown <= 0 {
		cooldown = 300
	}
	if a.LastWakeAt > 0 && now-a.LastWakeAt < cooldown {
		return
	}
	s.mu.Lock()
	if s.waking[peerID] {
		s.mu.Unlock()
		return
	}
	s.waking[peerID] = true
	s.mu.Unlock()
	go s.autoWake(a.ID, peerID)
}

func (s *Service) autoWake(accountID, peerID string) {
	defer func() {
		s.mu.Lock()
		delete(s.waking, peerID)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := s.Wake(ctx, accountID)
	now := s.now().Unix()
	detail := fmt.Sprintf("peer=%s account=%s ok=false", peerID, accountID)
	if err != nil {
		detail += " error=" + err.Error()
	} else {
		detail = fmt.Sprintf("peer=%s account=%s ok=%v vm=%s wake_ok=%v wake_state=%s error=%s wake_error=%s",
			peerID, accountID, res.OK, res.VMState, res.WakeOK, res.WakeState, res.Error, res.WakeError)
	}
	_ = s.st.AppendAudit("system", "muse.auto_wake", truncate(detail, 500), now)
	log.Printf("muse: auto-wake %s", truncate(detail, 300))
}

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{
		Running:     s.running,
		LoopEnabled: s.opts.KeepaliveInterval > 0,
		LastRun:     s.lastRun,
		NextRun:     s.nextRun,
		LastResult:  s.lastResult,
	}
}

func (s *Service) openAccount(a *store.MuseAccount) (map[string]string, map[string]int64, error) {
	if a.CookiesEnc == "" {
		return nil, nil, ErrMissingCookies
	}
	raw, err := decryptCookies(s.key, a.CookiesEnc)
	if err != nil {
		return nil, nil, err
	}
	var cookies map[string]string
	if err := json.Unmarshal(raw, &cookies); err != nil {
		return nil, nil, fmt.Errorf("invalid cookie JSON: %w", err)
	}
	if missing := missingEssential(cookies); len(missing) > 0 {
		return cookies, parseCookiesExp(a.CookiesExp), fmt.Errorf("%w: %s", ErrMissingCookies, strings.Join(missing, ", "))
	}
	return cookies, parseCookiesExp(a.CookiesExp), nil
}

func (s *Service) openCookies(a *store.MuseAccount) (map[string]string, error) {
	if a.CookiesEnc == "" {
		return map[string]string{}, nil
	}
	raw, err := decryptCookies(s.key, a.CookiesEnc)
	if err != nil {
		return nil, err
	}
	var cookies map[string]string
	if err := json.Unmarshal(raw, &cookies); err != nil {
		return nil, err
	}
	return cookies, nil
}

// promoteLegacyWakeError moves pre-split wake messages out of LastError. Older
// builds wrote a failed wake into the account-level error; the text is kept in
// LastWakeError (never discarded) so an upgrade does not keep showing a
// session alert for a healthy session.
func promoteLegacyWakeError(a *store.MuseAccount) {
	if a == nil || a.LastError == "" {
		return
	}
	legacy := false
	for _, prefix := range []string{
		"wake not confirmed (",
		"wake verification failed:",
		"muse wake HTTP ",
		"muse session has no vm_id; wake skipped",
		"muse VM is DISABLED; wake skipped",
	} {
		if strings.HasPrefix(a.LastError, prefix) {
			legacy = true
			break
		}
	}
	if !legacy {
		return
	}
	if a.LastWakeError == "" {
		a.LastWakeError = a.LastError
	}
	if a.LastWakeState == "" {
		a.LastWakeState = "unconfirmed"
	}
	// A legacy wake failure must not inherit an earlier successful wake's
	// LastWakeOK, or wake_recovered would claim "已上线" for it.
	a.LastWakeOK = false
	a.LastError = ""
}

func (s *Service) toAccount(a *store.MuseAccount) *Account {
	promoteLegacyWakeError(a)
	v := &Account{
		ID: a.ID, PeerID: a.PeerID, Label: a.Label,
		AuthState: a.AuthState, SessionStatus: a.SessionStatus,
		VMID: a.VMID, VMState: a.VMState, VMStateAt: a.VMStateAt,
		LastKeepaliveAt: a.LastKeepaliveAt, LastAttemptAt: a.LastAttemptAt,
		LastAttemptOK: a.LastAttemptOK, LastError: a.LastError,
		LastWakeAt: a.LastWakeAt, LastWakeOK: a.LastWakeOK,
		LastWakeError: a.LastWakeError, LastWakeState: a.LastWakeState,
		AutoWake: a.AutoWake, Enabled: a.Enabled,
		AuthorizedAt: a.AuthorizedAt, UpdatedAt: a.UpdatedAt,
	}
	v.CookiesExp = parseCookiesExp(a.CookiesExp)
	v.ExpiresAt = minEssentialExpiry(v.CookiesExp)
	cookies, err := s.openCookies(a)
	if err != nil {
		v.AuthState = "needs_reauth"
		if v.LastError == "" {
			v.LastError = "cookie unavailable: " + err.Error()
		}
		return v
	}
	v.CookieNames = sortedCookieNames(cookies)
	v.EssentialOK = len(missingEssential(cookies)) == 0
	return v
}

func applyRenew(a *store.MuseAccount, res *renewResult, now int64, recordWake bool) {
	promoteLegacyWakeError(a)
	if res.CookiesEnc != "" {
		a.CookiesEnc = res.CookiesEnc
	}
	if res.CookiesExpJSON != "" {
		a.CookiesExp = res.CookiesExpJSON
	}

	// A transient upstream failure (403/429/5xx, timeout, bad JSON) says
	// nothing about the account: keep the last confirmed auth/VM state and
	// the healthy row label. The error still travels in Result.Error and the
	// keepalive verdict log. A 200 non-assigned response is not transient: it
	// is a confirmed session failure, but never a reason to demand re-auth.
	transient := res.Transient && a.AuthState == "ok"
	if res.OK {
		// A successful session is authoritative, including an empty
		// vm_state (normalized to UNKNOWN by renew). Overwriting prevents a
		// stale value from surviving a sandbox transition.
		a.SessionStatus = res.SessionStatus
		a.VMID = res.VMID
		a.VMState = normalizeVMState(res.VMState)
		a.VMStateAt = res.VMStateAt
	} else if !transient {
		if res.SessionStatus != "" {
			a.SessionStatus = res.SessionStatus
		}
		if res.VMID != "" {
			a.VMID = res.VMID
		}
		if res.VMState != "" {
			a.VMState = res.VMState
		}
	}
	if res.AuthState != "" {
		a.AuthState = res.AuthState
	}
	if !transient {
		a.LastError = truncate(res.Error, 500)
		a.LastAttemptOK = res.OK
	}
	a.LastAttemptAt = res.At
	if res.OK {
		a.LastKeepaliveAt = res.At
	}
	// A manual Wake is a recorded event even when the session check fails
	// before the wake request: it must not leave an older successful wake
	// looking current. Keepalives only touch wake diagnostics when they
	// actually reach the wake step.
	if recordWake {
		a.LastWakeAt = res.At
		a.LastWakeOK = res.WakeOK
		if res.WakeState == "" {
			res.WakeState = "unconfirmed"
			if res.WakeError == "" {
				res.WakeError = "wake skipped: session check failed"
			}
		}
	}
	// Wake diagnostics are scoped to the wake step, so a session failure does
	// not erase the last known wake diagnosis (and vice versa).
	if res.WakeState != "" {
		a.LastWakeState = res.WakeState
		a.LastWakeError = truncate(res.WakeError, 500)
	}
	a.UpdatedAt = now
}

func toResult(a *store.MuseAccount, res *renewResult) *Result {
	auth := res.AuthState
	if auth == "" {
		auth = a.AuthState
	}
	vm, vmAt := res.VMState, res.VMStateAt
	if vm == "" {
		// Keep the last confirmed state in the response to a transient
		// failure; the row itself keeps it too (P2-2).
		vm, vmAt = a.VMState, a.VMStateAt
	}
	return &Result{
		ID: a.ID, PeerID: a.PeerID, Label: a.Label,
		OK: res.OK, AuthState: auth,
		SessionStatus: res.SessionStatus, VMID: res.VMID, VMState: vm,
		VMStateAt:     vmAt,
		WakeRequested: res.WakeRequested, WakeOK: res.WakeOK,
		ExpiresAt: minEssentialExpiry(res.Exp), Error: res.Error,
		WakeError: res.WakeError, WakeState: res.WakeState, At: res.At,
	}
}

func newAccountID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("ma_%d", time.Now().UnixNano())
	}
	return "ma_" + hex.EncodeToString(b[:])
}

func normalizeLabel(label, peerID string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return peerID
	}
	return truncate(label, 200)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func validateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("muse: base URL is not valid")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return errors.New("muse: base URL must be http(s)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return errors.New("muse: base URL must be an origin only")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return errors.New("muse: base URL must use https unless loopback")
	}
	return nil
}

func validateProxyURL(u *url.URL) error {
	if u.Host == "" {
		return errors.New("muse: proxy URL is not valid")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return errors.New("muse: proxy URL must be http(s) or socks5")
	}
	if u.Fragment != "" {
		return errors.New("muse: proxy URL must not carry a fragment")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

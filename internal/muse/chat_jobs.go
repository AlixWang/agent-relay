package muse

// chat_jobs.go is the v1 direct-chat job orchestrator (D1-D26). It keeps the
// governance gates, the in-memory job lease/capacity bookkeeping, the send and
// history polling state machine and the metadata-only audit trail in one
// place. No chat text, reply text, cookie or token ever enters audit_log or a
// server log line.

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	mrand "math/rand"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/AlixWang/agent-relay/internal/store"
)

// ChatStatus* are the job states in the POST/GET snapshots (design §5.3).
const (
	ChatStatusPreparing       = "preparing"
	ChatStatusSending         = "sending"
	ChatStatusConfirming      = "confirming"
	ChatStatusWaiting         = "waiting"
	ChatStatusReplied         = "replied"
	ChatStatusTimeout         = "timeout"
	ChatStatusDeliveryUnknown = "delivery_unknown"
	ChatStatusReplyUnknown    = "reply_unknown"
	ChatStatusFailed          = "failed"
	ChatStatusCancelled       = "cancelled"
)

const (
	chatJobRetention = time.Hour
	chatJobMapMax    = 256
	chatIdemTTL      = 10 * time.Minute
	chatMinBudget    = 30 * time.Second
	chatMaxBlock     = 20 * time.Second
	chatBusyWindow   = 10 * time.Minute
	chatAuditDetail  = 500
)

var chatSessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// ChatError is the safe error shape in a job snapshot. It carries codes,
// stages and retry hints only; upstream bodies never reach it.
type ChatError struct {
	Code           string `json:"code"`
	Class          string `json:"class,omitempty"`
	Retryable      bool   `json:"retryable"`
	RetryAfterSecs int    `json:"retry_after_secs,omitempty"`
}

// ChatAdmissionError is returned by StartChat before a job exists. The HTTP
// status is meaningful only at this layer (D21).
type ChatAdmissionError struct {
	Status         int
	Code           string
	RetryAfterSecs int
	JobID          string
}

func (e *ChatAdmissionError) Error() string {
	if e == nil {
		return "chat admission error"
	}
	return e.Code
}

// ChatStartRequest is the validated public POST body. Actor is filled by the
// admin gateway; it is never accepted from JSON.
type ChatStartRequest struct {
	PeerID          string `json:"peer_id"`
	Text            string `json:"text"`
	SessionID       string `json:"session_id"`
	BudgetSecs      int    `json:"budget_secs"`
	BlockSecs       int    `json:"block_secs"`
	Mode            string `json:"mode"`
	ClientRequestID string `json:"client_request_id"`
	Actor           string `json:"-"`
}

// ChatJobView is the console-facing snapshot. It deliberately has no text
// hash, request id or internal pointers.
type ChatJobView struct {
	ChatID       string     `json:"chat_id"`
	Status       string     `json:"status"`
	PeerID       string     `json:"peer_id"`
	AccountID    string     `json:"account_id"`
	Session      string     `json:"session"`
	Visibility   string     `json:"visibility"`
	BudgetSecs   int        `json:"budget_secs"`
	BaselineSeq  int64      `json:"baseline_seq"`
	UserSeq      int64      `json:"user_seq"`
	SentAt       int64      `json:"sent_at"`
	ConfirmedAt  int64      `json:"confirmed_at"`
	DeadlineAt   int64      `json:"deadline_at"`
	Reply        *ChatReply `json:"reply"`
	Confidence   string     `json:"confidence"`
	Warnings     []string   `json:"warnings"`
	PollCount    int        `json:"poll_count"`
	LastSeq      int64      `json:"last_seq"`
	Error        *ChatError `json:"error"`
	PartialText  string     `json:"partial_text,omitempty"`
	PartialDraft bool       `json:"draft,omitempty"`
	Duplicate    bool       `json:"duplicate"`
	Cancel       string     `json:"cancel,omitempty"`
	CreatedAt    int64      `json:"created_at"`
	UpdatedAt    int64      `json:"updated_at"`
}

// ChatStatusView is the settings/status payload (design §5.1).
type ChatStatusView struct {
	DeployEnabled     bool `json:"deploy_enabled"`
	RuntimeEnabled    bool `json:"runtime_enabled"`
	Active            int  `json:"active"`
	Capacity          int  `json:"capacity"`
	Jobs              int  `json:"jobs"`
	DefaultBudgetSecs int  `json:"default_budget_secs"`
	MaxBudgetSecs     int  `json:"max_budget_secs"`
	PollSecs          int  `json:"poll_secs"`
	PerPeerPerMinute  int  `json:"per_peer_per_minute"`
	PerPeerPerHour    int  `json:"per_peer_per_hour"`
	GlobalPerMinute   int  `json:"global_per_minute"`
}

// ChatJob is one in-memory dialogue. Terminal jobs stay for one hour for
// console polling (D19); they are never persisted.
type ChatJob struct {
	ID         string
	AccountID  string
	PeerID     string
	SessionID  string
	Session    string
	Visibility string
	Budget     time.Duration
	Text       string
	TextLen    int
	TextHash   string
	NodeID     string
	RequestID  string
	Actor      string
	CreatedAt  int64

	mu       sync.Mutex
	Status   string
	Baseline int64
	UserSeq  int64
	SentAt   int64
	Confirm  int64
	Deadline int64
	Reply    *ChatReply
	Conf     string
	Warnings []string
	Partial  string
	Draft    bool
	Err      *ChatError
	Polls    int
	LastSeq  int64
	Canceled bool
	Cancel   string
	DupUser  bool
	Updated  int64

	session chatSession
	cancel  context.CancelFunc
	done    chan struct{}
}

type chatIdem struct {
	jobID   string
	hash    string
	expires int64
}

// StartChat is the admission path: it validates text/fields, applies the
// three-layer gate, reserves one lease + one global slot, records rate tokens,
// creates the job and starts a worker that does not depend on the HTTP
// request context. block_secs only waits for the snapshot; it never extends
// the upstream budget (D20).
func (s *Service) StartChat(ctx context.Context, req ChatStartRequest) (*ChatJobView, error) {
	if s == nil {
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}
	text := normalizeChatText(req.Text)
	if text == "" {
		return nil, admissionErr(400, "text_empty", 0)
	}
	if n := len([]rune(text)); n > 4000 || len(text) > 16*1024 {
		return nil, admissionErr(400, "text_too_long", 0)
	}
	peerID := strings.TrimSpace(req.PeerID)
	if peerID == "" {
		return nil, admissionErr(400, "unknown_member", 0)
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID != "" && !chatSessionIDPattern.MatchString(sessionID) {
		return nil, admissionErr(400, "invalid_session_id", 0)
	}
	budget, block, _, err := s.chatRequestLimits(req)
	if err != nil {
		return nil, err
	}
	requestID := strings.TrimSpace(req.ClientRequestID)
	if len(requestID) > 64 {
		return nil, admissionErr(400, "invalid_request_id", 0)
	}
	hash := chatTextHash(text)
	now := s.now()
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "admin"
	}

	// Idempotency hit is answered before the capacity/lease gates (design
	// §5.2). The final reservation re-checks it under chatMu so two racing
	// requests with one id cannot both start.
	if requestID != "" {
		if v, conflict := s.lookupChatIdem(peerID, requestID, hash, now.Unix()); conflict != nil {
			return nil, conflict
		} else if v != nil {
			v.Duplicate = true
			return v, nil
		}
	}

	s.chatMu.Lock()
	deploy := s.opts.DirectChatEnabled
	runtime := s.chatRuntime
	s.chatMu.Unlock()
	if !deploy || !runtime {
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}

	peer, perr := s.st.GetPeer(peerID)
	if perr != nil {
		return nil, &ChatAdmissionError{Status: 500, Code: "muse_store_error"}
	}
	if peer == nil {
		return nil, admissionErr(404, "unknown_member", 0)
	}
	account, aerr := s.st.GetMuseAccountByPeer(peerID)
	if aerr != nil {
		return nil, &ChatAdmissionError{Status: 500, Code: "muse_store_error"}
	}
	if account == nil {
		return nil, admissionErr(404, "unknown_muse_account", 0)
	}
	if !account.ChatEnabled {
		return nil, admissionErr(409, "muse_chat_access_required", 0)
	}
	if !account.Enabled {
		return nil, admissionErr(409, "muse_account_disabled", 0)
	}
	switch account.AuthState {
	case "needs_reauth":
		return nil, admissionErr(409, "muse_needs_reauth", 0)
	case "", "unverified":
		return nil, admissionErr(409, "muse_account_unverified", 0)
	}
	if account.VMState == "DISABLED" {
		return nil, admissionErr(503, "muse_vm_disabled", 0)
	}

	s.chatMu.Lock()
	s.gcChatLocked(now.Unix())
	if requestID != "" {
		key := chatIdemKey(peerID, requestID)
		if e, ok := s.chatRequests[key]; ok && e.expires >= now.Unix() {
			if e.hash != hash {
				s.chatMu.Unlock()
				return nil, admissionErr(409, "idempotency_conflict", 0)
			}
			if v := s.viewChatLocked(e.jobID); v != nil {
				s.chatMu.Unlock()
				v.Duplicate = true
				return v, nil
			}
			// The referenced job has been GC'd but its idempotency window
			// has not. Drop the stale key and admit this request.
			delete(s.chatRequests, key)
		}
	}
	if !s.chatRuntime {
		s.chatMu.Unlock()
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}
	if existing := s.chatActive[account.ID]; existing != "" {
		s.chatMu.Unlock()
		return nil, &ChatAdmissionError{Status: 409, Code: "muse_chat_in_progress", JobID: existing}
	}
	select {
	case s.chatSem <- struct{}{}:
	default:
		s.chatMu.Unlock()
		return nil, admissionErr(429, "muse_chat_busy", 5)
	}
	if retry, code := s.checkChatRateLocked(peerID, now.Unix()); code != "" {
		<-s.chatSem
		s.chatMu.Unlock()
		return nil, admissionErr(429, code, retry)
	}
	if !s.makeChatRoomLocked(now.Unix()) {
		<-s.chatSem
		s.chatMu.Unlock()
		return nil, admissionErr(429, "muse_chat_busy", 5)
	}

	id := newChatID()
	visibility := "member_main_chat"
	if sessionID != "" {
		visibility = "member_side_chat"
	}
	hardCtx, hardCancel := context.WithTimeout(context.Background(), budget+30*time.Second)
	job := &ChatJob{
		ID: id, AccountID: account.ID, PeerID: peerID,
		SessionID: sessionID, Session: "main", Visibility: visibility,
		Budget: budget, Text: text, TextHash: hash, NodeID: chatNodeID(account.ID),
		RequestID: requestID, Actor: actor, CreatedAt: now.Unix(),
		Status: ChatStatusPreparing, Updated: now.Unix(),
		cancel: hardCancel, done: make(chan struct{}),
	}
	job.TextLen = len([]rune(text))
	if sessionID != "" {
		job.Session = sessionID
	}
	s.chatJobs[id] = job
	s.chatActive[account.ID] = id
	if requestID != "" {
		s.chatRequests[chatIdemKey(peerID, requestID)] = chatIdem{
			jobID: id, hash: hash, expires: now.Add(chatIdemTTL).Unix(),
		}
	}
	s.chatMu.Unlock()

	_ = s.st.SetMuseChatStatus(account.ID, now.Unix(), ChatStatusPreparing)
	go s.runChatWorker(hardCtx, job)

	if block > 0 {
		timer := time.NewTimer(block)
		select {
		case <-job.done:
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
	}
	return job.view(false), nil
}

func (s *Service) chatRequestLimits(req ChatStartRequest) (time.Duration, time.Duration, string, error) {
	budgetSecs := req.BudgetSecs
	if budgetSecs == 0 {
		budgetSecs = int(s.opts.ChatDefaultBudget / time.Second)
		if budgetSecs <= 0 {
			budgetSecs = int(chatDefaultBudgetHard / time.Second)
		}
	}
	minBudget := int(chatMinBudget / time.Second)
	maxBudget := int(s.opts.ChatMaxBudget / time.Second)
	if maxBudget <= 0 {
		maxBudget = int(chatMaxBudgetCeiling / time.Second)
	}
	if budgetSecs < minBudget || budgetSecs > maxBudget {
		return 0, 0, "", admissionErr(400, "budget_out_of_range", 0)
	}
	blockSecs := req.BlockSecs
	if blockSecs < 0 || blockSecs > int(chatMaxBlock/time.Second) {
		return 0, 0, "", admissionErr(400, "block_out_of_range", 0)
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode == "" {
		mode = "async"
	}
	switch mode {
	case "async":
		blockSecs = 0
	case "wait":
		// block_secs still caps the HTTP wait at 20s.
	default:
		return 0, 0, "", admissionErr(400, "invalid_mode", 0)
	}
	return time.Duration(budgetSecs) * time.Second, time.Duration(blockSecs) * time.Second, mode, nil
}

func admissionErr(status int, code string, retry int) *ChatAdmissionError {
	return &ChatAdmissionError{Status: status, Code: code, RetryAfterSecs: retry}
}

func chatIdemKey(peerID, requestID string) string { return peerID + "\x00" + requestID }

func (s *Service) lookupChatIdem(peerID, requestID, hash string, now int64) (*ChatJobView, *ChatAdmissionError) {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	s.gcChatLocked(now)
	e, ok := s.chatRequests[chatIdemKey(peerID, requestID)]
	if !ok || e.expires < now {
		return nil, nil
	}
	if e.hash != hash {
		return nil, admissionErr(409, "idempotency_conflict", 0)
	}
	if v := s.viewChatLocked(e.jobID); v != nil {
		return v, nil
	}
	return nil, nil
}

// viewChatLocked returns a snapshot while chatMu is held. Callers must not
// hold it across network I/O.
func (s *Service) viewChatLocked(id string) *ChatJobView {
	j := s.chatJobs[id]
	if j == nil {
		return nil
	}
	return j.view(false)
}

// GetChat returns one snapshot; terminal jobs remain for one hour (D19).
func (s *Service) GetChat(id string) (*ChatJobView, error) {
	now := s.now().Unix()
	s.chatMu.Lock()
	s.gcChatLocked(now)
	job := s.chatJobs[id]
	s.chatMu.Unlock()
	if job == nil {
		return nil, ErrChatNotFound
	}
	return job.view(false), nil
}

// ChatStatus reports the two global switches and live counters.
func (s *Service) ChatStatus() ChatStatusView {
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	v := ChatStatusView{
		DeployEnabled:     s.opts.DirectChatEnabled,
		RuntimeEnabled:    s.chatRuntime,
		Active:            len(s.chatActive),
		Capacity:          cap(s.chatSem),
		Jobs:              len(s.chatJobs),
		DefaultBudgetSecs: int(s.opts.ChatDefaultBudget / time.Second),
		MaxBudgetSecs:     int(s.opts.ChatMaxBudget / time.Second),
		PollSecs:          int(s.opts.ChatPollInterval / time.Second),
		PerPeerPerMinute:  s.opts.ChatPerPeerPerMin,
		PerPeerPerHour:    s.opts.ChatPerPeerPerHour,
		GlobalPerMinute:   s.opts.ChatGlobalPerMinute,
	}
	return v
}

// SetChatRuntime flips the runtime kill switch (D22). Enabling is only legal
// when the deploy flag was set at startup; disabling never sends chat.cancel
// to Muse, because a kill switch must not fan out into a burst of RPCs.
func (s *Service) SetChatRuntime(enabled bool, actor string) (ChatStatusView, error) {
	if s == nil {
		return ChatStatusView{}, admissionErr(503, "muse_chat_disabled", 0)
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "admin"
	}
	s.chatMu.Lock()
	if enabled && !s.opts.DirectChatEnabled {
		s.chatMu.Unlock()
		return s.ChatStatus(), admissionErr(409, "deploy_flag_off", 0)
	}
	s.chatRuntime = enabled
	active := 0
	var ids []string
	if !enabled {
		active = len(s.chatActive)
		for _, id := range s.chatActive {
			ids = append(ids, id)
		}
	}
	s.chatMu.Unlock()

	now := s.now().Unix()
	if err := s.st.AppendAudit(actor, "muse.chat.runtime_flag",
		fmt.Sprintf("enabled=%v by=%s", enabled, chatAuditValue(actor)), now); err != nil {
		log.Printf("muse chat: audit runtime_flag failed: %v", err)
	}
	if !enabled {
		detail := fmt.Sprintf("active_jobs=%d reason=kill_switch", active)
		if err := s.st.AppendAudit(actor, "muse.chat.disabled", detail, now); err != nil {
			log.Printf("muse chat: audit disabled failed: %v", err)
		}
		for _, id := range ids {
			s.chatMu.Lock()
			job := s.chatJobs[id]
			s.chatMu.Unlock()
			if job != nil {
				s.stopChat(job, actor, false)
			}
		}
	}
	return s.ChatStatus(), nil
}

// SetChatAccess writes the per-member administrator switch and its audit row
// atomically. Access follows the verified identity binding: a new cookie or a
// changed peer resets it elsewhere. This method only records the operator's
// action; it cannot bypass that reset.
func (s *Service) SetChatAccess(accountID string, enabled bool, reason, actor string) (*Account, error) {
	if s == nil {
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "admin"
	}
	a, err := s.st.GetMuseAccount(accountID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, ErrNotFound
	}
	now := s.now().Unix()
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "console"
	}
	if enabled {
		if !s.opts.DirectChatEnabled {
			return nil, admissionErr(409, "deploy_flag_off", 0)
		}
		if !a.Enabled {
			return nil, admissionErr(409, "muse_account_disabled", 0)
		}
		if a.AuthState == "needs_reauth" {
			return nil, admissionErr(409, "muse_needs_reauth", 0)
		}
		if a.AuthState != "ok" {
			return nil, admissionErr(409, "muse_account_unverified", 0)
		}
		detail := fmt.Sprintf("account=%s peer=%s by=%s reason=%s source=console",
			chatAuditValue(a.ID), chatAuditValue(a.PeerID), chatAuditValue(actor), chatAuditValue(reason))
		if err := s.st.SetMuseChatAccess(a.ID, true, now, actor,
			actor, "muse.chat.access_grant", truncate(detail, chatAuditDetail), now); err != nil {
			return nil, err
		}
	} else {
		detail := fmt.Sprintf("account=%s peer=%s by=%s reason=%s source=console",
			chatAuditValue(a.ID), chatAuditValue(a.PeerID), chatAuditValue(actor), chatAuditValue(reason))
		if err := s.st.SetMuseChatAccess(a.ID, false, now, "",
			actor, "muse.chat.access_revoke", truncate(detail, chatAuditDetail), now); err != nil {
			return nil, err
		}
	}
	if !enabled {
		go s.cancelChatsForAccount(a.ID, "access_revoke")
	}
	return s.Get(a.ID)
}

// auditChatAccessReset records a system-driven reset (new cookie or peer
// rebinding). It is separate from the account-row save and only logs on
// failure: the security invariant is the zeroed column, not the audit row.
func (s *Service) auditChatAccessReset(a *store.MuseAccount, reason string, now int64) {
	if a == nil {
		return
	}
	detail := fmt.Sprintf("account=%s peer=%s reason=%s",
		chatAuditValue(a.ID), chatAuditValue(a.PeerID), chatAuditValue(reason))
	if err := s.st.AppendAudit("system", "muse.chat.access_reset",
		truncate(detail, chatAuditDetail), now); err != nil {
		log.Printf("muse chat: audit access_reset failed for %s: %v", a.ID, err)
	}
}

// CancelChat stops waiting and best-effort cancels the generation. It never
// deletes the already-sent user message (D13).
func (s *Service) CancelChat(ctx context.Context, id, actor string) (*ChatJobView, error) {
	s.chatMu.Lock()
	job := s.chatJobs[id]
	s.chatMu.Unlock()
	if job == nil {
		return nil, ErrChatNotFound
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "admin"
	}
	s.chatMu.Lock()
	s.gcChatLocked(s.now().Unix())
	s.chatMu.Unlock()
	v, ok := s.stopChat(job, actor, true)
	if !ok {
		return nil, ErrChatNotActive
	}
	return v, nil
}

// stopChat finalizes a job at most once. sendCancel is false for a kill
// switch (design §2.7/§3.2) and true for an explicit cancel / opt-out.
func (s *Service) stopChat(job *ChatJob, actor string, sendCancel bool) (*ChatJobView, bool) {
	job.mu.Lock()
	if isChatTerminal(job.Status) {
		job.mu.Unlock()
		return job.view(false), false
	}
	job.Canceled = true
	job.Cancel = "skipped"
	sess := job.session
	cancel := job.cancel
	job.mu.Unlock()

	result := "skipped"
	if sendCancel && sess != nil {
		cctx, ccancel := context.WithTimeout(context.Background(), 4*time.Second)
		res, err := sess.Cancel(cctx, job.SessionID)
		ccancel()
		switch {
		case err != nil:
			result = "skipped"
		case res.Sent:
			result = "sent"
		case res.Unsupported:
			result = "unsupported"
		}
	}
	job.mu.Lock()
	job.Cancel = result
	job.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	now := s.now().Unix()
	out := chatOutcome{
		Status: ChatStatusCancelled,
		Error:  &ChatError{Code: "muse_cancelled", Class: "cancel", Retryable: false},
		Cancel: result,
		Actor:  actor,
	}
	if job.finish(out, now) {
		s.auditOutcome(job, out, now)
		s.recordChatStatus(job, now)
	}
	return job.view(false), true
}

func (s *Service) cancelChatsForAccount(accountID, reason string) {
	s.chatMu.Lock()
	var ids []string
	for _, id := range s.chatActive {
		job := s.chatJobs[id]
		if job != nil && job.AccountID == accountID {
			ids = append(ids, id)
		}
	}
	s.chatMu.Unlock()
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = s.CancelChat(ctx, id, "system")
		cancel()
	}
	_ = reason
}

// chatLeaseActive reports whether a live dialogue owns this account. Keepalive
// and wake call it before and after taking opMu (design §2.8/§4.4).
func (s *Service) chatLeaseActive(accountID string) bool {
	if s == nil || accountID == "" {
		return false
	}
	s.chatMu.Lock()
	defer s.chatMu.Unlock()
	return s.chatActive[accountID] != ""
}

func (s *Service) runChatWorker(ctx context.Context, job *ChatJob) {
	defer func() {
		job.mu.Lock()
		sess := job.session
		job.session = nil
		cancel := job.cancel
		job.mu.Unlock()
		if sess != nil {
			sess.Close()
		}
		if cancel != nil {
			cancel()
		}
		s.releaseChat(job)
		close(job.done)
	}()

	if job.cancelRequested() {
		out := chatOutcome{Status: ChatStatusCancelled,
			Error: &ChatError{Code: "muse_cancelled", Class: "cancel"}}
		if job.finish(out, s.now().Unix()) {
			s.auditOutcome(job, out, s.now().Unix())
		}
		return
	}
	out := s.driveChat(ctx, job)
	if !job.finish(out, s.now().Unix()) {
		return
	}
	s.auditOutcome(job, out, s.now().Unix())
	s.recordChatStatus(job, s.now().Unix())
}

func (s *Service) openChatSession(ctx context.Context, accountID string) (chatSession, error) {
	if s.chatOpen != nil {
		return s.chatOpen.Open(ctx, accountID)
	}
	return s.Open(ctx, accountID)
}

func (s *Service) driveChat(ctx context.Context, job *ChatJob) chatOutcome {
	if job.cancelRequested() {
		return cancelledOutcome(job)
	}
	sess, err := s.openChatSession(ctx, job.AccountID)
	if err != nil {
		if job.cancelRequested() || ctx.Err() != nil && errors.Is(ctx.Err(), context.Canceled) {
			return cancelledOutcome(job)
		}
		cerr := chatErrorFromFailure(err)
		return chatOutcome{Status: ChatStatusFailed, Error: cerr, Stage: cerr.Class}
	}
	job.setSession(sess)
	if live, ok := sess.(*liveChatSession); ok {
		live.setJobContext(job.ID, job.PeerID)
	}
	if job.cancelRequested() {
		return cancelledOutcome(job)
	}

	job.advance(ChatStatusSending, s.now().Unix())
	baselineEvents, berr := sess.History(ctx, job.SessionID, 5, 0)
	if berr != nil {
		if job.cancelRequested() {
			return cancelledOutcome(job)
		}
		cf := chatFailureAs(berr)
		reason := cf.Code
		if reason == "" {
			reason = "muse_upstream_error"
		}
		line := fmt.Sprintf("muse chat: baseline history failed chat=%s peer=%s account=%s stage=sending error_class=muse_history_unavailable reason=%s",
			chatAuditValue(job.ID), chatAuditValue(job.PeerID), chatAuditValue(job.AccountID),
			chatAuditValue(reason))
		if cf.Stage != "" {
			line += " failure_stage=" + chatAuditValue(cf.Stage)
		}
		if cf.Status > 0 {
			line += fmt.Sprintf(" status=%d", cf.Status)
		}
		log.Printf("%s", line)
		cerr := chatErrorFromFailure(berr)
		cerr.Code = "muse_history_unavailable"
		return chatOutcome{Status: ChatStatusFailed, Error: cerr, Stage: "sending",
			FailureReason: reason, FailureStatus: cf.Status}
	}
	baseline := maxChatSeq(baselineEvents.ChatEvents)
	job.setBaseline(baseline)
	if job.SessionID != "" && baseline == 0 && len(baselineEvents.ChatEvents) == 0 {
		return chatOutcome{Status: ChatStatusFailed, Stage: "sending",
			Error: &ChatError{Code: "unknown_session", Class: "sending"}}
	}
	if chatBusy(baselineEvents.ChatEvents, s.now()) {
		return chatOutcome{Status: ChatStatusFailed, Stage: "sending",
			Error: &ChatError{Code: "muse_agent_busy", Class: "sending", Retryable: true, RetryAfterSecs: 30}}
	}

	if aerr := s.auditSending(job); aerr != nil {
		log.Printf("muse chat: audit sending failed for %s: %v", job.ID, aerr)
		return chatOutcome{Status: ChatStatusFailed, Stage: "send",
			Error: &ChatError{Code: "muse_audit_unavailable", Class: "send"}}
	}
	if job.cancelRequested() {
		return cancelledOutcome(job)
	}
	job.setSentAt(s.now().Unix())
	sendRes, serr := sess.Send(ctx, job.SessionID, job.Text, job.NodeID)
	if job.cancelRequested() {
		return cancelledOutcome(job)
	}
	sendAddr := sendRes.Addr
	if sendAddr == "" {
		sendAddr = chatSendAddr(job.SessionID)
	}
	sendErr := sendRes.FailureCode
	if sendErr == "" && serr != nil {
		sendErr = chatFailureCode(serr)
	}
	if sendRes.Evidence != chatSendMayHaveArrived {
		// A definite rejection (or a failure before any write) must fail the
		// job instead of running the 5s confirmation window. Keep the
		// classified reason and HTTP status for the failed audit.
		cf := chatFailureAs(serr)
		if cf == nil {
			cf = &chatFailure{Code: "muse_upstream_error", Stage: "send"}
		}
		stage := cf.Stage
		if stage == "" {
			stage = "send"
		}
		status := sendRes.Status
		if status == 0 {
			status = cf.Status
		}
		reason := sendErr
		if reason == "" {
			reason = cf.Code
		}
		return chatOutcome{
			Status: ChatStatusFailed, Error: chatErrorFromFailure(serr), Stage: stage,
			FailureReason: reason, FailureStatus: status,
			SendStatus: sendRes.Status, SendEcho: sendRes.Echo, SendErr: sendErr,
			SendAddr: sendAddr, ResolvedFingerprint: sendRes.ResolvedFingerprint,
		}
	}
	job.advance(ChatStatusConfirming, s.now().Unix())
	seq, duplicate, confirm := s.confirmChat(ctx, sess, job)
	if job.cancelRequested() {
		return cancelledOutcome(job)
	}
	if seq <= 0 {
		return chatOutcome{
			Status: ChatStatusDeliveryUnknown, Stage: "confirm",
			Error:      &ChatError{Code: "muse_delivery_unknown", Class: "confirm"},
			SendStatus: sendRes.Status, SendEcho: sendRes.Echo, SendErr: sendErr,
			SendAddr: sendAddr, ConfirmReads: confirm.Reads,
			ConfirmErrors: confirm.Errors, LastConfirmErr: confirm.LastErr,
			ResolvedFingerprint: sendRes.ResolvedFingerprint,
		}
	}
	job.setConfirmed(seq, s.now().Unix(), duplicate)

	job.setDeadline(job.Confirmed() + int64(job.Budget/time.Second))
	job.advance(ChatStatusWaiting, s.now().Unix())
	now := s.now().Unix()
	echo := "no"
	if sendRes.Echo {
		echo = "yes"
	}
	sendEcho := "no"
	if sendRes.Echo {
		sendEcho = "yes"
	}
	detail := fmt.Sprintf("chat=%s echo=%s user_seq=%d delivery=confirmed elapsed_ms=%d send_status=%d send_echo=%s send_addr=%s resolved=%s",
		job.ID, echo, seq, elapsedMS(job, now), sendRes.Status, sendEcho,
		chatAuditValue(sendAddr), chatAuditValue(resolvedFingerprintOrUnknown(sendRes.ResolvedFingerprint)))
	if err := s.st.AppendAudit("system", "muse.chat.sent", truncate(detail, chatAuditDetail), now); err != nil {
		log.Printf("muse chat: audit sent failed for %s: %v", job.ID, err)
	}
	_ = s.st.SetMuseChatStatus(job.AccountID, now, ChatStatusWaiting)
	return s.waitForReply(ctx, sess, job)
}

// confirmChat waits up to 5s for our message.user to appear in history. The
// reply budget starts only once delivery is confirmed (D7/D12). Reads counts
// every confirmation read attempt; Errors and LastErr classify failures so the
// delivery_unknown audit can distinguish "no event" from "could not read".
func (s *Service) confirmChat(ctx context.Context, sess chatSession, job *ChatJob) (int64, bool, chatConfirmStats) {
	var stats chatConfirmStats
	cctx, cancel := context.WithTimeout(ctx, chatConfirmBudget)
	defer cancel()
	for {
		if job.cancelRequested() || cctx.Err() != nil {
			return 0, false, stats
		}
		stats.Reads++
		hist, err := sess.History(cctx, job.SessionID, 20, 0)
		if err == nil {
			if seq, dup, found := findChatUser(hist.ChatEvents, job.BaselineValue(), job.Text); found {
				return seq, dup, stats
			}
			job.bumpLastSeq(maxChatSeq(hist.ChatEvents))
		} else {
			stats.Errors++
			stats.LastErr = chatFailureCode(err)
			if errors.Is(err, context.Canceled) {
				return 0, false, stats
			}
		}
		timer := time.NewTimer(chatConfirmEvery)
		select {
		case <-cctx.Done():
			timer.Stop()
			return 0, false, stats
		case <-timer.C:
		}
	}
}

// waitForReply polls history every ~2s. Three consecutive transient failures
// trigger one read-only reconnect; a second failure or a seq regression ends
// the job without pretending a reply exists (design §2.5/§5.5).
func (s *Service) waitForReply(ctx context.Context, sess chatSession, job *ChatJob) chatOutcome {
	poll := s.opts.ChatPollInterval
	if poll <= 0 || poll > chatPollHard {
		poll = chatPollHard
	}
	deadline := job.DeadlineValue()
	lastSeq := job.LastSeqValue()
	consecutive := 0
	reconnects := 0
	var lastParse chatParseResult
	stats := chatPollStats{}

	for {
		if job.cancelRequested() {
			return cancelledOutcome(job)
		}
		if s.now().Unix() >= deadline {
			return timeoutOutcome(lastParse, stats)
		}

		events, maxSeq, err := s.pollChatHistory(ctx, sess, job)
		job.bumpPolls()
		if maxSeq > 0 {
			job.bumpLastSeq(maxSeq)
		}
		if err != nil {
			stats.HistoryErrors++
			if job.cancelRequested() {
				return cancelledOutcome(job)
			}
			if ctx.Err() != nil {
				return timeoutOutcome(lastParse, stats)
			}
			consecutive++
			if consecutive >= 3 {
				if reconnects >= 1 {
					return chatOutcome{Status: ChatStatusReplyUnknown, Stage: "poll",
						Error: &ChatError{Code: "muse_reply_unknown", Class: "poll", Retryable: true}}
				}
				reconnects++
				if old := job.currentSession(); old != nil {
					job.setSession(nil)
					old.Close()
				}
				next, oerr := s.openChatSession(ctx, job.AccountID)
				if oerr != nil {
					return chatOutcome{Status: ChatStatusReplyUnknown, Stage: "poll",
						Error: &ChatError{Code: "muse_reply_unknown", Class: "poll", Retryable: true}}
				}
				job.setSession(next)
				sess = next
				consecutive = 0
			}
			if !s.sleepChatPoll(ctx, poll) {
				if job.cancelRequested() {
					return cancelledOutcome(job)
				}
				return timeoutOutcome(lastParse, stats)
			}
			continue
		}
		consecutive = 0
		stats.HistoryReads++
		if maxSeq > lastSeq {
			stats.NewEventPolls++
			stats.LastSeqAdvanced = true
		}
		for _, ev := range events {
			if ev.Seq <= job.BaselineValue() || ev.EventName != "message.assistant" {
				continue
			}
			stats.AssistantEvents++
			if ev.Payload.Status == "" {
				stats.DraftEvents++
			}
		}

		if maxSeq > 0 && lastSeq > 0 && maxSeq < lastSeq {
			return chatOutcome{Status: ChatStatusFailed, Stage: "poll",
				Error: &ChatError{Code: "muse_history_reset", Class: "poll"}}
		}
		lastSeq = maxSeq

		parse := parseChatEvents(events, job.BaselineValue(), job.SessionID, job.UserSeqValue())
		lastParse = parse
		if parse.DraftText != "" {
			job.setPartial(parse.DraftText, true)
		}
		if parse.Reply != nil {
			conf := "high"
			warnings := append([]string(nil), parse.Warnings...)
			if parse.Ambiguous || job.userDuplicated() {
				conf = "ambiguous"
				warnings = append(warnings, "duplicate_user_text")
			}
			return chatOutcome{
				Status: ChatStatusReplied, Reply: parse.Reply,
				Confidence: conf, Warnings: uniqueStrings(warnings), Stage: "poll",
			}
		}
		if !s.sleepChatPoll(ctx, poll) {
			if job.cancelRequested() {
				return cancelledOutcome(job)
			}
			return timeoutOutcome(lastParse, stats)
		}
	}
}

func (s *Service) sleepChatPoll(ctx context.Context, base time.Duration) bool {
	timer := time.NewTimer(chatPollDelay(base))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func chatPollDelay(base time.Duration) time.Duration {
	if base <= 0 {
		base = chatPollHard
	}
	jitter := base / 10
	if jitter <= 0 {
		return base
	}
	delta := time.Duration(mrand.Int63n(int64(2 * jitter)))
	d := base - jitter + delta
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}

func (s *Service) pollChatHistory(ctx context.Context, sess chatSession, job *ChatJob) ([]chatEvent, int64, error) {
	hist, err := sess.History(ctx, job.SessionID, 20, 0)
	if err != nil {
		return nil, 0, err
	}
	events := append([]chatEvent(nil), hist.ChatEvents...)
	maxSeq := maxChatSeq(events)
	pages := 1
	for pages < 3 {
		minSeq := minChatSeq(events)
		if minSeq <= job.BaselineValue()+1 || !hist.HasMore {
			break
		}
		older, oerr := sess.History(ctx, job.SessionID, 50, minSeq)
		if oerr != nil {
			// A partial window is still usable; do not fail the whole poll.
			break
		}
		if len(older.ChatEvents) == 0 {
			break
		}
		events = append(events, older.ChatEvents...)
		hist = older
		pages++
	}
	return events, maxSeq, nil
}

func (s *Service) auditSending(job *ChatJob) error {
	detail := fmt.Sprintf("chat=%s peer=%s account=%s session=%s baseline=%d text_len=%d text_sha256=%s request_id=%s access=1 send_addr=%s",
		chatAuditValue(job.ID), chatAuditValue(job.PeerID), chatAuditValue(job.AccountID),
		chatAuditValue(job.Session), job.BaselineValue(), job.TextLen,
		job.TextHash, chatAuditValue(job.RequestID), chatAuditValue(chatSendAddr(job.SessionID)))
	return s.st.AppendAudit(job.Actor, "muse.chat.sending", truncate(detail, chatAuditDetail), s.now().Unix())
}

func (s *Service) auditOutcome(job *ChatJob, o chatOutcome, now int64) {
	action := ""
	detail := ""
	actor := o.Actor
	if actor == "" {
		actor = "system"
	}
	switch o.Status {
	case ChatStatusReplied:
		action = "muse.chat.replied"
		replySeq, replyID, replyStatus := int64(0), "", ""
		if o.Reply != nil {
			replySeq, replyID, replyStatus = o.Reply.Seq, o.Reply.MessageID, o.Reply.Status
		}
		detail = fmt.Sprintf("chat=%s reply_seq=%d reply_id=%s reply_status=%s elapsed_ms=%d polls=%d confidence=%s",
			chatAuditValue(job.ID), replySeq, chatAuditValue(replyID), chatAuditValue(replyStatus),
			elapsedMS(job, now), job.PollsValue(), o.Confidence)
	case ChatStatusTimeout:
		action = "muse.chat.timeout"
		partial := "no"
		if o.Partial != "" {
			partial = "yes"
		}
		advanced := "no"
		if o.PollStats.LastSeqAdvanced {
			advanced = "yes"
		}
		detail = fmt.Sprintf("chat=%s partial=%s last_seq=%d elapsed_ms=%d polls=%d history_reads=%d history_errors=%d new_event_polls=%d last_seq_advanced=%s assistant_events=%d draft_events=%d",
			chatAuditValue(job.ID), partial, job.LastSeqValue(), elapsedMS(job, now), job.PollsValue(),
			o.PollStats.HistoryReads, o.PollStats.HistoryErrors, o.PollStats.NewEventPolls,
			advanced, o.PollStats.AssistantEvents, o.PollStats.DraftEvents)
	case ChatStatusDeliveryUnknown:
		action = "muse.chat.delivery_unknown"
		echo := "no"
		if o.SendEcho {
			echo = "yes"
		}
		sendErr := o.SendErr
		if sendErr == "" {
			sendErr = "none"
		}
		lastConfirmErr := o.LastConfirmErr
		if lastConfirmErr == "" {
			lastConfirmErr = "none"
		}
		addr := o.SendAddr
		if addr == "" {
			addr = chatSendAddr(job.SessionID)
		}
		detail = fmt.Sprintf("chat=%s reason=no_user_event last_seq=%d send_status=%d send_echo=%s send_err=%s send_addr=%s confirm_reads=%d confirm_errors=%d last_confirm_err=%s resolved=%s",
			chatAuditValue(job.ID), job.LastSeqValue(), o.SendStatus, echo,
			chatAuditValue(sendErr), chatAuditValue(addr), o.ConfirmReads, o.ConfirmErrors,
			chatAuditValue(lastConfirmErr),
			chatAuditValue(resolvedFingerprintOrUnknown(o.ResolvedFingerprint)))
	case ChatStatusCancelled:
		action = "muse.chat.cancelled"
		detail = fmt.Sprintf("chat=%s cancel=%s", chatAuditValue(job.ID), o.Cancel)
	case ChatStatusFailed:
		action = "muse.chat.failed"
		stage := o.Stage
		code := ""
		if o.Error != nil {
			code = o.Error.Code
			if stage == "" {
				stage = o.Error.Class
			}
		}
		detail = fmt.Sprintf("chat=%s stage=%s error_class=%s",
			chatAuditValue(job.ID), chatAuditValue(stage), chatAuditValue(code))
		if o.FailureReason != "" {
			detail += " reason=" + chatAuditValue(o.FailureReason)
		}
		if o.FailureStatus > 0 {
			detail += fmt.Sprintf(" status=%d", o.FailureStatus)
		}
	case ChatStatusReplyUnknown:
		action = "muse.chat.failed"
		detail = fmt.Sprintf("chat=%s stage=poll error_class=muse_reply_unknown",
			chatAuditValue(job.ID))
	}
	if action == "" {
		return
	}
	if err := s.st.AppendAudit(actor, action, truncate(detail, chatAuditDetail), now); err != nil {
		log.Printf("muse chat: audit %s failed for %s: %v", action, job.ID, err)
	}
}

func (s *Service) recordChatStatus(job *ChatJob, now int64) {
	status := job.statusValue()
	if status == "" {
		return
	}
	if err := s.st.SetMuseChatStatus(job.AccountID, now, status); err != nil {
		log.Printf("muse chat: status write failed for %s: %v", job.ID, err)
	}
}

func (s *Service) releaseChat(job *ChatJob) {
	s.chatMu.Lock()
	if s.chatActive[job.AccountID] == job.ID {
		delete(s.chatActive, job.AccountID)
	}
	select {
	case <-s.chatSem:
	default:
	}
	s.chatMu.Unlock()
}

func (s *Service) gcChatLocked(now int64) {
	for id, job := range s.chatJobs {
		if isChatTerminal(job.statusValue()) && now-job.updatedValue() > int64(chatJobRetention/time.Second) {
			delete(s.chatJobs, id)
		}
	}
	for key, e := range s.chatRequests {
		if e.expires < now {
			delete(s.chatRequests, key)
		}
	}
}

// makeChatRoomLocked enforces the 256-job map ceiling by evicting the oldest
// terminal job. Active jobs are never evicted (D19).
func (s *Service) makeChatRoomLocked(now int64) bool {
	if len(s.chatJobs) < chatJobMapMax {
		return true
	}
	s.gcChatLocked(now)
	if len(s.chatJobs) < chatJobMapMax {
		return true
	}
	var oldest *ChatJob
	for _, job := range s.chatJobs {
		if !isChatTerminal(job.statusValue()) {
			continue
		}
		if oldest == nil || job.updatedValue() < oldest.updatedValue() {
			oldest = job
		}
	}
	if oldest == nil {
		return false
	}
	delete(s.chatJobs, oldest.ID)
	return true
}

func (s *Service) checkChatRateLocked(peerID string, now int64) (int, string) {
	cutMin, cutHour := now-60, now-3600
	s.chatPerMin[peerID] = pruneChatTimes(s.chatPerMin[peerID], cutMin)
	s.chatPerHour[peerID] = pruneChatTimes(s.chatPerHour[peerID], cutHour)
	s.chatGlobal = pruneChatTimes(s.chatGlobal, cutMin)

	if len(s.chatPerMin[peerID]) >= s.opts.ChatPerPeerPerMin {
		return retryAfterChat(s.chatPerMin[peerID], 60, now), "muse_chat_rate_limited"
	}
	if len(s.chatPerHour[peerID]) >= s.opts.ChatPerPeerPerHour {
		return retryAfterChat(s.chatPerHour[peerID], 3600, now), "muse_chat_rate_limited"
	}
	if len(s.chatGlobal) >= s.opts.ChatGlobalPerMinute {
		return retryAfterChat(s.chatGlobal, 60, now), "muse_chat_rate_limited"
	}
	s.chatPerMin[peerID] = append(s.chatPerMin[peerID], now)
	s.chatPerHour[peerID] = append(s.chatPerHour[peerID], now)
	s.chatGlobal = append(s.chatGlobal, now)
	return 0, ""
}

func pruneChatTimes(in []int64, cutoff int64) []int64 {
	if len(in) == 0 {
		return in
	}
	i := 0
	for i < len(in) && in[i] <= cutoff {
		i++
	}
	if i == 0 {
		return in
	}
	return append([]int64(nil), in[i:]...)
}

func retryAfterChat(in []int64, window int64, now int64) int {
	if len(in) == 0 {
		return 1
	}
	oldest := in[0]
	for _, v := range in {
		if v < oldest {
			oldest = v
		}
	}
	n := int(oldest + window - now)
	if n < 1 {
		n = 1
	}
	return n
}

func chatBusy(events []chatEvent, now time.Time) bool {
	events = dedupeAndSortEvents(events)
	if len(events) > 5 {
		events = events[len(events)-5:]
	}
	if len(events) == 0 {
		return false
	}
	last := events[len(events)-1]
	if last.EventName == "message.assistant" && last.Payload.Status == "" {
		return true
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.EventName != "message.user" {
			continue
		}
		if now.UnixMilli()-ev.OccurredAtMS >= int64(chatBusyWindow/time.Millisecond) {
			return false
		}
		for j := i + 1; j < len(events); j++ {
			if events[j].EventName == "message.assistant" && events[j].Payload.Status != "" {
				return false
			}
		}
		return true
	}
	return false
}

func maxChatSeq(events []chatEvent) int64 {
	var max int64
	for _, ev := range events {
		if ev.Seq > max {
			max = ev.Seq
		}
	}
	return max
}

func minChatSeq(events []chatEvent) int64 {
	var min int64
	for _, ev := range events {
		if ev.Seq > 0 && (min == 0 || ev.Seq < min) {
			min = ev.Seq
		}
	}
	return min
}

func chatErrorFromFailure(err error) *ChatError {
	cf := chatFailureAs(err)
	if cf == nil {
		return &ChatError{Code: "muse_upstream_error", Retryable: true}
	}
	code := cf.Code
	if code == "" {
		code = "muse_upstream_error"
	}
	return &ChatError{
		Code: code, Class: cf.Stage,
		Retryable: cf.Retryable, RetryAfterSecs: cf.RetryAfter,
	}
}

type chatOutcome struct {
	Status     string
	Error      *ChatError
	Reply      *ChatReply
	Confidence string
	Warnings   []string
	Partial    string
	Draft      bool
	Stage      string
	Cancel     string
	Actor      string
	PollStats  chatPollStats

	// Send/confirm diagnostics are audit-only. SendStatus/SendEcho/SendErr/
	// SendAddr describe the write attempt; ConfirmReads/ConfirmErrors/
	// LastConfirmErr describe the delivery confirmation window;
	// ResolvedFingerprint is the 8-hex fingerprint of the resolved session id
	// (never the raw id).
	SendStatus          int
	SendEcho            bool
	SendErr             string
	SendAddr            string
	ConfirmReads        int
	ConfirmErrors       int
	LastConfirmErr      string
	ResolvedFingerprint string

	// FailureReason/FailureStatus preserve the underlying chatFailure for
	// audit only. They are never copied into ChatError (the public error code
	// stays muse_history_unavailable).
	FailureReason string
	FailureStatus int
}

type chatConfirmStats struct {
	Reads   int
	Errors  int
	LastErr string
}

type chatPollStats struct {
	HistoryReads    int
	HistoryErrors   int
	NewEventPolls   int
	AssistantEvents int
	DraftEvents     int
	LastSeqAdvanced bool
}

func cancelledOutcome(job *ChatJob) chatOutcome {
	result := "skipped"
	if job != nil {
		result = job.cancelValue()
	}
	return chatOutcome{Status: ChatStatusCancelled, Cancel: result,
		Error: &ChatError{Code: "muse_cancelled", Class: "cancel"}}
}

func timeoutOutcome(parse chatParseResult, stats chatPollStats) chatOutcome {
	return chatOutcome{
		Status: ChatStatusTimeout, Stage: "wait",
		Error:   &ChatError{Code: "muse_reply_timeout", Class: "wait", Retryable: true},
		Partial: parse.DraftText, Draft: parse.DraftText != "",
		Warnings:  parse.Warnings,
		PollStats: stats,
	}
}

func isChatTerminal(status string) bool {
	switch status {
	case ChatStatusReplied, ChatStatusTimeout, ChatStatusDeliveryUnknown,
		ChatStatusReplyUnknown, ChatStatusFailed, ChatStatusCancelled:
		return true
	}
	return false
}

func newChatID() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("mc_%016x", time.Now().UnixNano())
	}
	return "mc_" + hex.EncodeToString(b[:])
}

func chatAuditValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '=' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, s)
}

// resolvedFingerprintOrUnknown keeps the audit field present for synthetic
// sessions that do not carry a fingerprint, without ever accepting a raw id.
func resolvedFingerprintOrUnknown(fingerprint string) string {
	if fingerprint == "" {
		return "unknown"
	}
	return fingerprint
}

func elapsedMS(job *ChatJob, now int64) int64 {
	start := job.ConfirmValue()
	if start <= 0 {
		start = job.CreatedAt
	}
	if now < start {
		return 0
	}
	return (now - start) * 1000
}

func (j *ChatJob) view(duplicate bool) *ChatJobView {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := &ChatJobView{
		ChatID: j.ID, Status: j.Status, PeerID: j.PeerID, AccountID: j.AccountID,
		Session: j.Session, Visibility: j.Visibility, BudgetSecs: int(j.Budget / time.Second),
		BaselineSeq: j.Baseline, UserSeq: j.UserSeq, SentAt: j.SentAt,
		ConfirmedAt: j.Confirm, DeadlineAt: j.Deadline,
		Reply: j.Reply, Confidence: j.Conf, Warnings: append([]string(nil), j.Warnings...),
		PollCount: j.Polls, LastSeq: j.LastSeq, Error: j.Err,
		PartialText: j.Partial, PartialDraft: j.Draft,
		Duplicate: duplicate, Cancel: j.Cancel, CreatedAt: j.CreatedAt, UpdatedAt: j.Updated,
	}
	if v.Confidence == "" {
		v.Confidence = "unknown"
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	return v
}

func (j *ChatJob) finish(o chatOutcome, now int64) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if isChatTerminal(j.Status) {
		return false
	}
	if j.Canceled {
		o = chatOutcome{Status: ChatStatusCancelled, Cancel: firstNonEmpty(o.Cancel, j.Cancel),
			Error: &ChatError{Code: "muse_cancelled", Class: "cancel"}, Actor: o.Actor}
	}
	j.Status = o.Status
	j.Reply = o.Reply
	j.Conf = o.Confidence
	j.Warnings = append([]string(nil), o.Warnings...)
	j.Partial = o.Partial
	j.Draft = o.Draft
	j.Err = o.Error
	j.Cancel = firstNonEmpty(o.Cancel, j.Cancel)
	j.Updated = now
	return true
}

func (j *ChatJob) advance(status string, now int64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Canceled || isChatTerminal(j.Status) {
		return
	}
	j.Status = status
	if now > 0 {
		j.Updated = now
	}
}

func (j *ChatJob) setSession(sess chatSession) {
	j.mu.Lock()
	j.session = sess
	j.mu.Unlock()
}

func (j *ChatJob) currentSession() chatSession {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.session
}

func (j *ChatJob) setBaseline(seq int64) {
	j.mu.Lock()
	j.Baseline = seq
	if seq > j.LastSeq {
		j.LastSeq = seq
	}
	j.mu.Unlock()
}

func (j *ChatJob) setSentAt(sec int64) {
	j.mu.Lock()
	j.SentAt = sec
	j.mu.Unlock()
}

func (j *ChatJob) setConfirmed(seq, sec int64, duplicate bool) {
	j.mu.Lock()
	j.UserSeq = seq
	j.Confirm = sec
	j.DupUser = duplicate
	if seq > j.LastSeq {
		j.LastSeq = seq
	}
	j.mu.Unlock()
}

func (j *ChatJob) setDeadline(sec int64) {
	j.mu.Lock()
	j.Deadline = sec
	j.mu.Unlock()
}

func (j *ChatJob) setPartial(text string, draft bool) {
	j.mu.Lock()
	j.Partial = text
	j.Draft = draft
	j.mu.Unlock()
}

func (j *ChatJob) bumpPolls() {
	j.mu.Lock()
	j.Polls++
	j.mu.Unlock()
}

func (j *ChatJob) bumpLastSeq(seq int64) {
	if seq <= 0 {
		return
	}
	j.mu.Lock()
	if seq > j.LastSeq {
		j.LastSeq = seq
	}
	j.mu.Unlock()
}

func (j *ChatJob) cancelRequested() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Canceled
}

func (j *ChatJob) BaselineValue() int64 { j.mu.Lock(); defer j.mu.Unlock(); return j.Baseline }
func (j *ChatJob) UserSeqValue() int64  { j.mu.Lock(); defer j.mu.Unlock(); return j.UserSeq }
func (j *ChatJob) ConfirmValue() int64  { j.mu.Lock(); defer j.mu.Unlock(); return j.Confirm }
func (j *ChatJob) DeadlineValue() int64 { j.mu.Lock(); defer j.mu.Unlock(); return j.Deadline }
func (j *ChatJob) LastSeqValue() int64  { j.mu.Lock(); defer j.mu.Unlock(); return j.LastSeq }
func (j *ChatJob) PollsValue() int      { j.mu.Lock(); defer j.mu.Unlock(); return j.Polls }
func (j *ChatJob) userDuplicated() bool { j.mu.Lock(); defer j.mu.Unlock(); return j.DupUser }
func (j *ChatJob) ConfirmAt() int64     { j.mu.Lock(); defer j.mu.Unlock(); return j.Confirm }

func (j *ChatJob) statusValue() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Status
}

func (j *ChatJob) updatedValue() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Updated
}

func (j *ChatJob) cancelValue() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Cancel
}

// Confirmed exists for the orchestrator but must not shadow the public view.
func (j *ChatJob) Confirmed() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.Confirm
}

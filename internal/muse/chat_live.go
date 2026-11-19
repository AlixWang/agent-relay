package muse

// chat_live.go adapts the existing Noise transport to one direct-chat job.
// A job owns one short-lived connection (D11); chat.stream and chat.history
// share it through the chatConn single-reader multiplexer. No credential or
// upstream URL is ever copied into an error: errors are classified, logged
// and audited by code only.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

const (
	chatEchoBudget     = 10 * time.Second
	chatEchoMaxBytes   = 64 * 1024
	chatEchoMaxLines   = 100
	chatHistoryTimeout = 4 * time.Second
	chatDialTimeout    = 6 * time.Second
	chatHandshakeTMO   = 6 * time.Second
	chatSessionsPath   = "/api/session/list"
)

// Variables, not constants, so the orchestration tests can shorten the
// confirmation window and the session-list deadline without real 5s/4s sleeps.
// Tests in this package do not run in parallel.
var (
	chatConfirmBudget   = 5 * time.Second
	chatConfirmEvery    = 250 * time.Millisecond
	chatSessionsTimeout = 4 * time.Second
)

// liveChatSession is the production chatSession. Close is the only reliable
// way to end every logical stream on the connection; v1 never sends a stream
// reset (muse-cli does not use one).
type liveChatSession struct {
	cc        *chatConn
	accountID string

	// The daemon treats an omitted session_id differently for /chat/stream
	// (write the member main chat) and /chat/history (read no session). Resolve
	// the main chat to its explicit id once per connection and use it for both
	// directions; see resolveSession.
	mainMu        sync.Mutex
	mainSessionID string

	// The job stamps its identifiers here before the first History/Send so a
	// session-list failure can be logged with the chat/peer that caused it.
	// These are diagnostic only; they never affect addressing.
	logMu  sync.Mutex
	chatID string
	peerID string
}

func (l *liveChatSession) setJobContext(chatID, peerID string) {
	if l == nil {
		return
	}
	l.logMu.Lock()
	l.chatID, l.peerID = chatID, peerID
	l.logMu.Unlock()
}

func (l *liveChatSession) logContext() (chatID, peerID, accountID string) {
	if l == nil {
		return "?", "?", "?"
	}
	l.logMu.Lock()
	chatID, peerID = l.chatID, l.peerID
	l.logMu.Unlock()
	if chatID == "" {
		chatID = "?"
	}
	if peerID == "" {
		peerID = "?"
	}
	if l.accountID == "" {
		accountID = "?"
	} else {
		accountID = l.accountID
	}
	return chatID, peerID, accountID
}

// logSessionListFailure writes one metadata-only line. It deliberately carries
// no token, cookie, session id, request path body or upstream response body;
// shape is a count/flag summary (see sessionListShape).
func (l *liveChatSession) logSessionListFailure(cf *chatFailure, shape string) {
	if cf == nil {
		cf = newChatFailure("muse_upstream_error", "session_list", true)
	}
	chatID, peerID, accountID := l.logContext()
	code := cf.Code
	if code == "" {
		code = "muse_upstream_error"
	}
	line := fmt.Sprintf("muse chat: session list failed chat=%s peer=%s account=%s stage=session_list reason=%s",
		chatAuditValue(chatID), chatAuditValue(peerID), chatAuditValue(accountID), chatAuditValue(code))
	if cf.Status > 0 {
		line += fmt.Sprintf(" status=%d", cf.Status)
	}
	if shape != "" {
		line += " shape=" + shape
	}
	log.Printf("%s", line)
}

// sessionListShape summarizes only the response envelope shape. It never
// includes a session_id value: entries holds one two-character presence flag
// per row (P/p for is_primary, T/t for is_thread, - for absent).
func sessionListShape(list chatSessionList, resolvedID string) string {
	entries := list.entries()
	primaryField, threadField := 0, 0
	flags := make([]string, 0, len(entries))
	for _, e := range entries {
		p, t := "-", "-"
		if e.IsPrimary != nil {
			primaryField++
			if *e.IsPrimary {
				p = "P"
			} else {
				p = "p"
			}
		}
		if e.IsThread != nil {
			threadField++
			if *e.IsThread {
				t = "T"
			} else {
				t = "t"
			}
		}
		flags = append(flags, p+t)
	}
	resolved := "no"
	if resolvedID != "" {
		resolved = "yes"
	}
	flagText := "none"
	if len(flags) > 0 {
		flagText = strings.Join(flags, ",")
	}
	return fmt.Sprintf("sessions=%d primary_field=%d thread_field=%d entries=%s resolved=%s",
		len(entries), primaryField, threadField, flagText, resolved)
}

func (l *liveChatSession) Close() {
	if l == nil || l.cc == nil {
		return
	}
	l.cc.close()
}

// serviceCall is the probe-only door to another gateway service on this same
// connection. It is not used by chat.send/history/cancel.
func (l *liveChatSession) serviceCall(ctx context.Context, service int, verb, path string,
	body []byte) (*appResponse, error) {

	if l == nil || l.cc == nil {
		return nil, newNoiseError("probe", "connection_closed", true, nil)
	}
	return l.cc.callService(ctx, service, verb, path, body, nil)
}

func chatNodeID(accountID string) string {
	mac := hmac.New(sha256.New, []byte(accountID))
	mac.Write([]byte("agent-relay-muse-node"))
	return hex.EncodeToString(mac.Sum(nil))[:16]
}

type chatCredentials struct {
	token    string
	vmID     string
	endpoint string
	woke     bool
}

// Open implements chatGateway. It prepares one token and one connection. If
// the account's wake state is uncertain it wakes first; if a direct dial then
// fails it wakes once and redials. A second dial failure is reported as VM
// unavailable, never retried indefinitely.
func (s *Service) Open(ctx context.Context, accountID string) (chatSession, error) {
	creds, _, fail := s.prepareChatCredentials(ctx, accountID, false)
	if fail != nil {
		return nil, fail
	}
	for attempt := 0; attempt < 2; attempt++ {
		conn, err := s.dialChatConn(ctx, creds)
		if err == nil {
			cc := newChatConn(ctx, conn)
			return &liveChatSession{cc: cc, accountID: accountID}, nil
		}
		fail = chatFailureFromNoise("dial", err)
		if attempt == 0 && !creds.woke && fail.Retryable &&
			fail.Code != "muse_protocol_drift" {
			next, _, wfail := s.prepareChatCredentials(ctx, accountID, true)
			if wfail != nil {
				return nil, wfail
			}
			creds = next
			creds.woke = true
			continue
		}
		if fail.Code == "muse_upstream_error" || fail.Code == "muse_upstream_timeout" {
			fail.Code = "muse_vm_unavailable"
			fail.RetryAfter = 30
		}
		return nil, fail
	}
	return nil, newChatFailure("muse_vm_unavailable", "dial", true)
}

// prepareChatCredentials runs the one short HTTPS preparation under opMu. It
// persists rotated cookies even on failure (same rule as keepalive) and never
// updates last_keepalive_at: a chat preflight is not a keepalive. A wake that
// really executed still records last_wake_*/last_attempt_*, per D11/§4.5.
func (s *Service) prepareChatCredentials(ctx context.Context, accountID string, forceWake bool) (
	*chatCredentials, *store.MuseAccount, *chatFailure) {

	s.opMu.Lock()
	defer s.opMu.Unlock()

	a, err := s.st.GetMuseAccount(accountID)
	if err != nil {
		return nil, nil, newChatFailure("muse_store_error", "prepare", true)
	}
	if a == nil {
		return nil, nil, newChatFailure("unknown_muse_account", "prepare", false)
	}
	if !a.Enabled {
		return nil, a, newChatFailure("muse_account_disabled", "prepare", false)
	}
	if a.AuthState == "needs_reauth" {
		return nil, a, newChatFailure("muse_needs_reauth", "prepare", false)
	}
	cookies, exp, oerr := s.openAccount(a)
	if oerr != nil {
		a.AuthState = "needs_reauth"
		a.LastError = truncate("cookie unavailable: "+oerr.Error(), 500)
		a.UpdatedAt = s.now().Unix()
		_ = s.st.SaveMuseAccount(a)
		return nil, a, newChatFailure("muse_needs_reauth", "prepare", false)
	}

	needWake := forceWake || !a.LastWakeOK || a.LastWakeState == "unconfirmed" ||
		a.VMState == "DISABLED"
	prevKeepalive := a.LastKeepaliveAt
	res, rerr := s.renew(ctx, cookies, exp, needWake)
	now := s.now().Unix()
	if res != nil {
		applyRenew(a, res, now, needWake && res.WakeRequested)
		// applyRenew treats a successful renew as a keepalive. This path is
		// not one: restore the keepalive clock so the console's health view is
		// not washed by a chat.
		a.LastKeepaliveAt = prevKeepalive
		if serr := s.st.SaveMuseAccount(a); serr != nil {
			return nil, a, newChatFailure("muse_store_error", "prepare", true)
		}
		s.recordRetry(a.ID, res)
	}
	if rerr != nil {
		return nil, a, chatFailureFromPrepare(rerr)
	}
	if res == nil {
		return nil, a, newChatFailure("muse_upstream_error", "prepare", true)
	}
	if res.AuthState == "needs_reauth" {
		return nil, a, newChatFailure("muse_needs_reauth", "prepare", false)
	}
	if res.VMID == "" {
		return nil, a, newChatFailure("muse_vm_unavailable", "prepare", true)
	}
	if needWake && !res.WakeOK {
		cf := newChatFailure("muse_wake_failed", "prepare", true)
		cf.RetryAfter = 30
		return nil, a, cf
	}

	sess := &sessionResponse{
		Status: res.SessionStatus, VMID: res.VMID,
		VMState: res.VMState, EndpointURL: res.EndpointURL,
	}
	token, terr := s.fetchHatchToken(ctx, res, sess)
	if res.CookiesEnc != "" || res.CookiesExpJSON != "" {
		// fetchHatchToken may rotate cookies during its one 401 refresh.
		applyRenew(a, res, s.now().Unix(), needWake && res.WakeRequested)
		a.LastKeepaliveAt = prevKeepalive
		if serr := s.st.SaveMuseAccount(a); serr != nil {
			return nil, a, newChatFailure("muse_store_error", "prepare", true)
		}
	}
	if terr != nil {
		return nil, a, chatFailureFromPrepare(terr)
	}
	return &chatCredentials{
		token: token, vmID: res.VMID, endpoint: res.EndpointURL,
		woke: needWake && res.WakeOK,
	}, a, nil
}

func (s *Service) dialChatConn(ctx context.Context, creds *chatCredentials) (*noiseConn, error) {
	dctx, cancel := context.WithTimeout(ctx, chatDialTimeout)
	conn, err := dialNoiseConn(dctx, s.client, s.gatewayURL, creds.vmID, creds.token)
	cancel()
	if err != nil {
		return nil, err
	}
	if err := conn.handshake(ctx, chatHandshakeTMO); err != nil {
		conn.close()
		return nil, err
	}
	return conn, nil
}

func chatFailureFromPrepare(err error) *chatFailure {
	if err == nil {
		return nil
	}
	var cf *chatFailure
	if errors.As(err, &cf) {
		return cf
	}
	var he *MuseHTTPError
	if errors.As(err, &he) {
		switch he.StatusCode {
		case http.StatusUnauthorized:
			return newChatFailure("muse_needs_reauth", "prepare", false)
		case http.StatusForbidden:
			return newChatFailure("muse_egress_forbidden", "prepare", true)
		case http.StatusTooManyRequests:
			cf := newChatFailure("muse_upstream_rate_limited", "prepare", true)
			cf.Status = he.StatusCode
			cf.RetryAfter = ceilSeconds(he.RetryAfter)
			return cf
		default:
			cf := newChatFailure("muse_upstream_error", "prepare", true)
			cf.Status = he.StatusCode
			return cf
		}
	}
	var unavailable *MuseSessionUnavailable
	if errors.As(err, &unavailable) {
		cf := newChatFailure("muse_vm_unavailable", "prepare", true)
		cf.RetryAfter = 30
		return cf
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newChatFailure("muse_upstream_timeout", "prepare", true)
	}
	var ne *noiseError
	if errors.As(err, &ne) {
		switch {
		case ne.Class == "401" || ne.Class == "403":
			return newChatFailure("muse_needs_reauth", "prepare", false)
		case ne.Class == "bad_decrypt" || ne.Class == "rv_unsupported":
			return newChatFailure("muse_protocol_drift", "prepare", false)
		case ne.Stage == "dial" || ne.Stage == "handshake":
			cf := newChatFailure("muse_vm_unavailable", "prepare", true)
			cf.RetryAfter = 30
			return cf
		case ne.Stage == "wake":
			return newChatFailure("muse_wake_failed", "prepare", true)
		default:
			return newChatFailure("muse_upstream_error", "prepare", true)
		}
	}
	return newChatFailure("muse_upstream_error", "prepare", true)
}

func chatFailureFromNoise(stage string, err error) *chatFailure {
	if err == nil {
		return nil
	}
	var cf *chatFailure
	if errors.As(err, &cf) {
		return cf
	}
	var ne *noiseError
	if errors.As(err, &ne) {
		switch {
		case ne.Class == "401":
			return newChatFailure("muse_needs_reauth", stage, false)
		case ne.Class == "bad_decrypt" || ne.Class == "rv_unsupported":
			return newChatFailure("muse_protocol_drift", stage, false)
		case ne.Class == "timeout":
			return newChatFailure("muse_upstream_timeout", stage, true)
		case strings.HasPrefix(ne.Class, "reset"):
			return newChatFailure("muse_protocol_drift", stage, false)
		default:
			return newChatFailure("muse_upstream_error", stage, ne.Retryable)
		}
	}
	if errors.Is(err, context.Canceled) {
		return &chatFailure{Code: "muse_cancelled", Stage: stage}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newChatFailure("muse_upstream_timeout", stage, true)
	}
	return newChatFailure("muse_upstream_error", stage, true)
}

func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// chatSessionListEntry is the subset of GET /api/session/list the chat path
// needs. is_primary/is_thread are pointers because an older server shape may
// omit them; a missing field must not be mistaken for false.
type chatSessionListEntry struct {
	SessionID string `json:"session_id"`
	IsPrimary *bool  `json:"is_primary"`
	IsThread  *bool  `json:"is_thread"`
}

type chatSessionList struct {
	Sessions []chatSessionListEntry `json:"sessions"`
	// Result carries the gateway's wrapped response shape. muse-cli's
	// gateway.call_json accepts both {"sessions":[...]} and
	// {"result":{"sessions":[...]}} by returning d["result"] when present, so
	// the chat path must accept both as well. Only this one envelope layer is
	// documented; deeper or alternative envelopes are not guessed here.
	Result *chatSessionList `json:"result"`
}

// entries returns the effective sessions list, unwrapping the documented
// result envelope exactly once.
func (l chatSessionList) entries() []chatSessionListEntry {
	if l.Result != nil {
		return l.Result.Sessions
	}
	return l.Sessions
}

// sessionListTopKeys returns a sorted, value-free summary of the top-level JSON
// object keys. It is only used when the parsed list is empty, to show which
// envelope the upstream actually returned without logging any value.
func sessionListTopKeys(body []byte) string {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return "[]"
	}
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, chatAuditValue(k))
	}
	sort.Strings(keys)
	return "[" + strings.Join(keys, ",") + "]"
}

// mainChatSessionID picks the member main chat from a sessions list. The main
// chat is normally flagged is_primary=true (and is_thread=false); if neither
// flag is present in this server shape, the documented order puts it first.
// Ambiguous or missing metadata yields no id rather than guessing a side chat.
func mainChatSessionID(entries []chatSessionListEntry) string {
	var first string
	var unthreaded []string
	sawPrimary, sawThread := false, false
	for _, e := range entries {
		if e.SessionID == "" {
			continue
		}
		if first == "" {
			first = e.SessionID
		}
		if e.IsPrimary != nil {
			sawPrimary = true
			if *e.IsPrimary {
				return e.SessionID
			}
		}
		if e.IsThread != nil {
			sawThread = true
			if !*e.IsThread {
				unthreaded = append(unthreaded, e.SessionID)
			}
		}
	}
	if len(unthreaded) == 1 {
		return unthreaded[0]
	}
	if !sawPrimary && !sawThread {
		// The documented sessions.list order is main chat first.
		return first
	}
	return ""
}

// resolveSession maps the job's empty session id (member main chat) to the
// explicit id both /chat/stream and /chat/history understand. It is cached per
// connection; a side-chat id is returned unchanged without a lookup. A
// resolution failure is returned before any write, so the job fails as a
// send failure instead of silently sending and then reporting unknown.
func (l *liveChatSession) resolveSession(ctx context.Context, sessionID string) (string, error) {
	if sessionID != "" {
		return sessionID, nil
	}
	if l == nil || l.cc == nil {
		cf := newChatFailure("muse_upstream_error", "session_list", true)
		l.logSessionListFailure(cf, "unavailable")
		return "", cf
	}
	l.mainMu.Lock()
	defer l.mainMu.Unlock()
	if l.mainSessionID != "" {
		return l.mainSessionID, nil
	}

	rctx, cancel := context.WithTimeout(ctx, chatSessionsTimeout)
	resp, err := l.cc.call(rctx, http.MethodGet, chatSessionsPath, nil, nil)
	cancel()
	if err != nil {
		cf := chatFailureFromNoise("session_list", err)
		l.logSessionListFailure(cf, "unavailable")
		return "", cf
	}
	if resp.Status < 200 || resp.Status >= 300 {
		cf := chatStatusFailure("session_list", resp.Status, false)
		l.logSessionListFailure(cf, fmt.Sprintf("unparsed body_bytes=%d", len(resp.Body)))
		return "", cf
	}
	var list chatSessionList
	if err := json.Unmarshal(resp.Body, &list); err != nil {
		cf := newChatFailure("muse_protocol_drift", "session_list", false)
		l.logSessionListFailure(cf, fmt.Sprintf("invalid_json body_bytes=%d", len(resp.Body)))
		return "", cf
	}
	id := mainChatSessionID(list.entries())
	if id == "" {
		cf := newChatFailure("muse_main_chat_unresolved", "session_list", false)
		shape := sessionListShape(list, "")
		if len(list.entries()) == 0 {
			shape += " top_keys=" + sessionListTopKeys(resp.Body)
		}
		l.logSessionListFailure(cf, shape)
		return "", cf
	}
	l.mainSessionID = id
	return id, nil
}

func (l *liveChatSession) History(ctx context.Context, sessionID string, limit int, beforeSeq int64) (*chatHistoryResponse, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	sid, serr := l.resolveSession(ctx, sessionID)
	if serr != nil {
		return nil, serr
	}
	path := chatHistoryPath(sid, limit, beforeSeq)
	rctx, cancel := context.WithTimeout(ctx, chatHistoryTimeout)
	defer cancel()
	resp, err := l.cc.call(rctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, chatFailureFromNoise("history", err)
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, chatStatusFailure("history", resp.Status, true)
	}
	out, perr := parseChatHistory(resp.Body)
	if perr != nil {
		return nil, newChatFailure("muse_protocol_drift", "history", false)
	}
	return out, nil
}

func chatHistoryPath(sessionID string, limit int, beforeSeq int64) string {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if sessionID != "" {
		q.Set("session_id", sessionID)
	}
	if beforeSeq > 0 {
		q.Set("before_seq", strconv.FormatInt(beforeSeq, 10))
	}
	return "/chat/history?" + q.Encode()
}

func chatStatusFailure(stage string, status int, sessionScoped bool) *chatFailure {
	cf := newChatFailure("muse_protocol_drift", stage, false)
	cf.Status = status
	switch {
	case status == http.StatusUnauthorized:
		cf.Code = "muse_needs_reauth"
	case status == http.StatusForbidden:
		cf.Code = "muse_egress_forbidden"
		cf.Retryable = true
	case status == http.StatusNotFound && sessionScoped:
		cf.Code = "unknown_session"
	case status == http.StatusTooManyRequests:
		cf.Code = "muse_upstream_rate_limited"
		cf.Retryable = true
		cf.RetryAfter = 5
	case status >= 500:
		cf.Code = "muse_upstream_error"
		cf.Retryable = true
	}
	return cf
}

func (l *liveChatSession) Send(ctx context.Context, sessionID, text, nodeID string) (chatSendResult, error) {
	var res chatSendResult
	sid, serr := l.resolveSession(ctx, sessionID)
	if serr != nil {
		return res, serr
	}
	payload := map[string]any{
		"items":        []map[string]string{{"type": "text", "text": text}},
		"node_id":      nodeID,
		"capabilities": map[string]any{},
		"session_id":   sid,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return res, newChatFailure("muse_protocol_drift", "send", false)
	}
	sink, err := l.cc.openStream(ctx, http.MethodPost, "/chat/stream", body, nil)
	if err != nil {
		cf := chatFailureFromNoise("send", err)
		// The request may have reached the server before the write failed.
		cf.Wrote = true
		return res, cf
	}
	defer func() {
		// Terminal frames unregister themselves; this is idempotent for the
		// normal end and cleans up a timed-out echo sink.
		l.cc.unregisterStream(sink)
	}()

	waitCtx, cancel := context.WithTimeout(ctx, chatEchoBudget)
	defer cancel()
	var buf bytes.Buffer
	lines := 0
	status := 0
	echo := false
	for {
		select {
		case f := <-sink.ch:
			if f.Err != nil {
				cf := chatFailureFromNoise("send", f.Err)
				cf.Wrote = true
				return res, cf
			}
			if f.Reset != nil {
				cf := newChatFailure("muse_protocol_drift", "send", false)
				cf.Wrote = true
				cf.Status = status
				return res, cf
			}
			if f.Response != nil {
				echo = true
				status = f.Response.Status
				if status >= 400 {
					res.Wrote = true
					res.Status = status
					return res, chatStatusFailure("send", status, true)
				}
				if len(f.Response.Body) > 0 {
					buf.Write(f.Response.Body)
				}
				if f.Response.EndBody {
					res.Wrote = true
					res.Status = status
					res.Echo = echo
					lines += countFullLines(&buf, &res, true)
					res.EchoLines = lines
					return res, nil
				}
			}
			if f.BodyChunk != nil {
				echo = true
				if len(f.BodyChunk.Data) > 0 {
					buf.Write(f.BodyChunk.Data)
				}
				if buf.Len() > chatEchoMaxBytes || lines > chatEchoMaxLines {
					res.Wrote = true
					res.Status = status
					return res, newChatFailure("muse_stream_record_too_large", "send", false)
				}
				if f.BodyChunk.EndBody {
					res.Wrote = true
					res.Status = status
					res.Echo = echo
					lines += countFullLines(&buf, &res, true)
					res.EchoLines = lines
					return res, nil
				}
			}
		case <-waitCtx.Done():
			if ctx.Err() != nil {
				cf := newChatFailure("muse_cancelled", "send", false)
				cf.Wrote = true
				res.Wrote = true
				res.Status = status
				res.Echo = echo
				res.EchoLines = lines
				return res, cf
			}
			// §1.4: a stream that does not end within 10s is not a send
			// failure. Stop waiting for the opaque echo and let the job
			// confirm delivery from history.
			res.Wrote = true
			res.Status = status
			res.Echo = echo
			res.EchoLines = lines + countFullLines(&buf, &res, false)
			res.TimedOut = true
			return res, nil
		}
	}
}

// countFullLines consumes complete NDJSON lines. trailing controls whether a
// final unterminated record is accepted as a line (only at end_body).
func countFullLines(buf *bytes.Buffer, res *chatSendResult, trailing bool) int {
	data := buf.Bytes()
	n := 0
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(data[:i])
		if len(line) > 0 {
			n++
			if !json.Valid(line) {
				res.InvalidEcho = true
			}
		}
		data = data[i+1:]
	}
	if trailing {
		if line := bytes.TrimSpace(data); len(line) > 0 {
			n++
			if !json.Valid(line) {
				res.InvalidEcho = true
			}
		}
		data = nil
	}
	buf.Reset()
	if len(data) > 0 {
		buf.Write(data)
	}
	return n
}

func (l *liveChatSession) Cancel(ctx context.Context, sessionID string) (chatCancelResult, error) {
	var res chatCancelResult
	sid, serr := l.resolveSession(ctx, sessionID)
	if serr != nil {
		return res, serr
	}
	raw, err := json.Marshal(map[string]string{"session_id": sid})
	if err != nil {
		return res, newChatFailure("muse_protocol_drift", "cancel", false)
	}
	body := raw
	rctx, cancel := context.WithTimeout(ctx, noiseRPCTimeout)
	defer cancel()
	resp, err := l.cc.call(rctx, http.MethodPost, "/chat/cancel", body, nil)
	if err != nil {
		return res, chatFailureFromNoise("cancel", err)
	}
	res.Status = resp.Status
	if resp.Status >= 200 && resp.Status < 300 {
		res.Sent = true
		return res, nil
	}
	switch resp.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusMethodNotAllowed:
		res.Unsupported = true
		return res, nil
	case http.StatusUnauthorized:
		return res, newChatFailure("muse_needs_reauth", "cancel", false)
	default:
		return res, chatStatusFailure("cancel", resp.Status, true)
	}
}

// chatTextStats returns the Unicode code point count and SHA-256 of the
// normalized text. Only these values are ever written to audit_log (D5).
func chatTextStats(text string) (int, string) {
	norm := normalizeChatText(text)
	sum := sha256.Sum256([]byte(norm))
	return len([]rune(norm)), hex.EncodeToString(sum[:])
}

var _ chatGateway = (*Service)(nil)

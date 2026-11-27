package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

// mainChatUpstream models the behavior production exposed: an omitted
// session_id on POST /chat/stream writes the member main chat, while an
// explicit main-chat id is accepted but does not land (the 11-27 symptom).
// An omitted session_id on GET /chat/history reads no session and returns an
// empty history; the main chat is addressable only by the id from
// GET /api/session/list.
type mainChatUpstream struct {
	mainID string
	// listBody overrides the default bare sessions.list response. Tests use it
	// to exercise the documented {"result":{...}} gateway envelope.
	listBody []byte

	mu                 sync.Mutex
	sessionListCalls   int
	mainExplicitSends  int
	implicitSends      int
	sideSends          int
	lastSendSessionID  string
	lastSendHadSession bool
	nextSeq            int64
	events             map[string][]map[string]any
}

func newMainChatUpstream() *mainChatUpstream {
	return &mainChatUpstream{mainID: "main-1", events: map[string][]map[string]any{}}
}

func (m *mainChatUpstream) addEventLocked(sessionID, name, text string) int64 {
	m.nextSeq++
	seq := m.nextSeq
	ev := map[string]any{
		"event_name": name,
		"seq":        seq,
		"message_id": fmt.Sprintf("msg-%d", seq),
		"payload": map[string]any{
			"display_text": text,
			"content":      text,
			"session_id":   sessionID,
		},
	}
	m.events[sessionID] = append(m.events[sessionID], ev)
	return seq
}

func (m *mainChatUpstream) handle(req musetest.Request, _ *musetest.Emitter) musetest.Response {
	u, _ := url.Parse(req.Path)
	switch {
	case req.Verb == http.MethodGet && u.Path == chatSessionsPath:
		m.mu.Lock()
		m.sessionListCalls++
		m.mu.Unlock()
		if m.listBody != nil {
			return musetest.Response{Status: 200, Body: m.listBody, EndBody: true}
		}
		body, _ := json.Marshal(map[string]any{"sessions": []map[string]any{
			{"session_id": m.mainID, "title": "Main", "is_primary": true, "is_thread": false},
			{"session_id": "side-1", "title": "Side", "is_primary": false, "is_thread": true},
		}})
		return musetest.Response{Status: 200, Body: body, EndBody: true}
	case req.Verb == http.MethodPost && u.Path == "/chat/stream":
		var in struct {
			SessionID string `json:"session_id"`
			Items     []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"items"`
		}
		if err := json.Unmarshal(req.Body, &in); err != nil {
			return musetest.Response{Status: 400, Body: []byte(`{"error":"bad json"}`), EndBody: true}
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(req.Body, &raw); err != nil {
			return musetest.Response{Status: 400, Body: []byte(`{"error":"bad json"}`), EndBody: true}
		}
		_, hadSessionID := raw["session_id"]
		text := ""
		for _, item := range in.Items {
			if item.Type == "text" {
				text = item.Text
				break
			}
		}
		m.mu.Lock()
		sid := in.SessionID
		m.lastSendSessionID = sid
		m.lastSendHadSession = hadSessionID
		explicitMain := sid != "" && sid == m.mainID
		switch {
		case explicitMain:
			// 11-27 production shape: the explicit main-chat id is accepted
			// (200 + echo) but the message is not persisted in that session.
			m.mainExplicitSends++
		case sid == "":
			// Production behavior: no id means write the main chat.
			sid = m.mainID
			m.implicitSends++
		default:
			m.sideSends++
		}
		var seq int64
		if !explicitMain {
			seq = m.addEventLocked(sid, "message.user", text)
		}
		m.mu.Unlock()
		if explicitMain {
			echo, _ := json.Marshal(map[string]any{"message_id": "dropped-explicit-main"})
			return musetest.Response{Status: 200, Body: append(echo, '\n'), EndBody: true}
		}
		echo, _ := json.Marshal(map[string]any{"message_id": fmt.Sprintf("msg-%d", seq)})
		return musetest.Response{Status: 200, Body: append(echo, '\n'), EndBody: true}
	case req.Verb == http.MethodGet && u.Path == "/chat/history":
		sid := u.Query().Get("session_id")
		m.mu.Lock()
		var events []map[string]any
		if sid != "" {
			// Production behavior: no id returns an empty history, not main.
			events = append(events, m.events[sid]...)
		}
		m.mu.Unlock()
		body, _ := json.Marshal(map[string]any{"chat_events": events, "has_more": false})
		return musetest.Response{Status: 200, Body: body, EndBody: true}
	case req.Verb == http.MethodPost && u.Path == "/chat/cancel":
		return musetest.Response{Status: 200, Body: []byte(`{"ok":true}`), EndBody: true}
	default:
		return musetest.Response{Status: 404, EndBody: true}
	}
}

func (m *mainChatUpstream) counts() (sessionLists, mainExplicit, implicit, side int, lastSID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessionListCalls, m.mainExplicitSends, m.implicitSends, m.sideSends, m.lastSendSessionID
}

// lastSendAddress reports whether the last /chat/stream body carried a
// session_id key at all, plus its value. A lastSID of "" alone cannot tell an
// omitted key from an empty string, which is exactly the distinction 4.1 pins.
func (m *mainChatUpstream) lastSendAddress() (hadSessionID bool, lastSID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSendHadSession, m.lastSendSessionID
}

func newLiveChatSessionForTest(t *testing.T, wsURL string) *liveChatSession {
	t.Helper()
	cc := dialChatTestConn(t, wsURL)
	return &liveChatSession{cc: cc, accountID: "acct-test"}
}

func chatAuditField(t *testing.T, detail, field string) string {
	t.Helper()
	marker := " " + field + "="
	idx := strings.Index(detail, marker)
	if idx < 0 {
		t.Fatalf("audit detail %q has no %s field", detail, field)
	}
	rest := detail[idx+len(marker):]
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func TestLiveChatSessionUsesImplicitMainWriteAndExplicitHistory(t *testing.T) {
	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	sess := newLiveChatSessionForTest(t, ws.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The job's baseline read uses the same empty session id as the real one.
	base, err := sess.History(ctx, "", 5, 0)
	if err != nil {
		t.Fatalf("baseline history: %v", err)
	}
	if len(base.ChatEvents) != 0 {
		t.Fatalf("baseline should be empty, got %+v", base.ChatEvents)
	}

	res, err := sess.Send(ctx, "", "pwd", "node-1")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Evidence != chatSendMayHaveArrived || !res.Echo {
		t.Fatalf("send result: %+v", res)
	}
	if res.Addr != "implicit" {
		t.Fatalf("send addr=%q, want implicit", res.Addr)
	}
	if len(res.ResolvedFingerprint) != 8 {
		t.Fatalf("resolved fingerprint=%q, want 8 hex chars", res.ResolvedFingerprint)
	}

	hist, err := sess.History(ctx, "", 20, 0)
	if err != nil {
		t.Fatalf("confirm history: %v", err)
	}
	if len(hist.ChatEvents) != 1 {
		t.Fatalf("confirm history events=%+v (empty history is the defect)", hist.ChatEvents)
	}
	seq, dup, found := findChatUser(hist.ChatEvents, 0, "pwd")
	if !found || seq != 1 || dup {
		t.Fatalf("confirm: found=%v seq=%d dup=%v events=%+v", found, seq, dup, hist.ChatEvents)
	}

	sessionLists, mainExplicit, implicit, side, _ := m.counts()
	if sessionLists != 1 {
		t.Fatalf("session list calls=%d, want one cached lookup", sessionLists)
	}
	if mainExplicit != 0 || implicit != 1 || side != 0 {
		t.Fatalf("send address: explicit-main=%d implicit=%d side=%d", mainExplicit, implicit, side)
	}
	hadSessionID, sentSID := m.lastSendAddress()
	if hadSessionID || sentSID != "" {
		t.Fatalf("/chat/stream carried session_id=%q (present=%v); main chat must omit it", sentSID, hadSessionID)
	}
	for _, req := range ws.Requests() {
		if !strings.HasPrefix(req.Path, "/chat/history?") {
			continue
		}
		u, err := url.Parse(req.Path)
		if err != nil {
			t.Fatalf("history path: %v", err)
		}
		if got := u.Query().Get("session_id"); got != m.mainID {
			t.Fatalf("/chat/history session_id=%q, want explicit %q", got, m.mainID)
		}
	}
}

func TestLiveChatSessionSideChatIsNotRemapped(t *testing.T) {
	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	sess := newLiveChatSessionForTest(t, ws.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := sess.Send(ctx, "side-1", "side text", "node-1")
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if res.Addr != "explicit" || len(res.ResolvedFingerprint) != 8 {
		t.Fatalf("side send metadata: %+v", res)
	}
	hist, err := sess.History(ctx, "side-1", 20, 0)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist.ChatEvents) != 1 {
		t.Fatalf("side history: %+v", hist.ChatEvents)
	}
	sessionLists, mainExplicit, implicit, side, lastSID := m.counts()
	if sessionLists != 0 || mainExplicit != 0 || implicit != 0 || side != 1 || lastSID != "side-1" {
		t.Fatalf("side send used lookup: lists=%d main=%d implicit=%d side=%d last=%q",
			sessionLists, mainExplicit, implicit, side, lastSID)
	}
	if hadSessionID, _ := m.lastSendAddress(); !hadSessionID {
		t.Fatal("side chat /chat/stream must carry an explicit session_id")
	}
}

func TestLiveChatSessionUnresolvedMainChatNeverWrites(t *testing.T) {
	ws := musetest.New(t)
	streams := 0
	ws.SetStreamHandler(func(req musetest.Request, _ *musetest.Emitter) musetest.Response {
		switch {
		case req.Verb == http.MethodGet && req.Path == chatSessionsPath:
			return musetest.Response{Status: 200, Body: []byte(`{"sessions":[]}`), EndBody: true}
		case req.Verb == http.MethodPost && req.Path == "/chat/stream":
			streams++
			return musetest.Response{Status: 200, Body: []byte("{}\n"), EndBody: true}
		default:
			return musetest.Response{Status: 404, EndBody: true}
		}
	})
	sess := newLiveChatSessionForTest(t, ws.URL)

	result, err := sess.Send(context.Background(), "", "must not send", "node-1")
	if err == nil {
		t.Fatal("send succeeded without a resolvable main chat")
	}
	cf := chatFailureAs(err)
	if cf.Code != "muse_main_chat_unresolved" || result.Evidence != chatSendNotAttempted {
		t.Fatalf("failure: %+v", cf)
	}
	if result.Addr != "implicit" || result.FailureCode != "muse_main_chat_unresolved" || result.ResolvedFingerprint != "" {
		t.Fatalf("send metadata: %+v", result)
	}
	if streams != 0 {
		t.Fatalf("wrote a stream without a main-chat id: %d", streams)
	}
}

func TestMainChatSessionIDSelection(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		name string
		in   []chatSessionListEntry
		want string
	}{
		{"primary", []chatSessionListEntry{
			{SessionID: "side", IsPrimary: &no, IsThread: &yes},
			{SessionID: "main", IsPrimary: &yes, IsThread: &no},
		}, "main"},
		{"unthreaded", []chatSessionListEntry{
			{SessionID: "side", IsThread: &yes},
			{SessionID: "main", IsThread: &no},
		}, "main"},
		{"documented first", []chatSessionListEntry{
			{SessionID: "main"}, {SessionID: "side"},
		}, "main"},
		{"no main", []chatSessionListEntry{
			{SessionID: "side", IsPrimary: &no, IsThread: &yes},
		}, ""},
		{"ambiguous unthreaded", []chatSessionListEntry{
			{SessionID: "a", IsThread: &no}, {SessionID: "b", IsThread: &no},
		}, ""},
		{"null id skipped", []chatSessionListEntry{
			{SessionID: ""}, {SessionID: "main", IsPrimary: &yes},
		}, "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mainChatSessionID(tc.in); got != tc.want {
				t.Fatalf("mainChatSessionID=%q want %q", got, tc.want)
			}
		})
	}
}

// liveChatGateway returns the same production session, so the orchestrator
// exercises the real send/history address resolution.
type liveChatGateway struct {
	sess chatSession
}

func TestChatSendEvidenceThroughNoise(t *testing.T) {
	oldBudget, oldEvery := chatConfirmBudget, chatConfirmEvery
	chatConfirmBudget, chatConfirmEvery = 250*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { chatConfirmBudget, chatConfirmEvery = oldBudget, oldEvery })
	for _, tc := range []struct {
		name, wantStatus, wantCode string
		status                     int
		landed, disconnect         bool
	}{
		{"reset_after_landing", ChatStatusWaiting, "", 0, true, false},
		{"reset_without_history", ChatStatusDeliveryUnknown, "muse_delivery_unknown", 0, false, false},
		{"connection_lost_after_write", ChatStatusDeliveryUnknown, "muse_delivery_unknown", 0, true, true},
		{"ambiguous_502_after_landing", ChatStatusWaiting, "", 502, true, false},
		{"ambiguous_500_without_landing", ChatStatusDeliveryUnknown, "muse_delivery_unknown", 500, false, false},
		{"ambiguous_500_after_landing", ChatStatusWaiting, "", 500, true, false},
		{"bad_request", ChatStatusFailed, "muse_protocol_drift", 400, false, false},
		{"unauthorized", ChatStatusFailed, "muse_needs_reauth", 401, false, false},
		{"forbidden", ChatStatusFailed, "muse_egress_forbidden", 403, false, false},
		{"unknown_session", ChatStatusFailed, "unknown_session", 404, false, false},
		{"method_not_allowed", ChatStatusFailed, "muse_protocol_drift", 405, false, false},
		{"request_timeout_ambiguous", ChatStatusDeliveryUnknown, "muse_delivery_unknown", 408, false, false},
		{"conflict", ChatStatusFailed, "muse_protocol_drift", 409, false, false},
		{"gone", ChatStatusFailed, "muse_protocol_drift", 410, false, false},
		{"unprocessable", ChatStatusFailed, "muse_protocol_drift", 422, false, false},
		{"rate_limited", ChatStatusFailed, "muse_upstream_rate_limited", 429, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newMainChatUpstream()
			ws := musetest.New(t)
			ws.SetStreamHandler(func(req musetest.Request, emit *musetest.Emitter) musetest.Response {
				if req.Path != "/chat/stream" {
					return upstream.handle(req, emit)
				}
				if tc.landed {
					upstream.handle(req, emit)
				}
				if tc.disconnect {
					return musetest.Response{Disconnect: true}
				}
				if tc.status != 0 {
					return musetest.Response{Status: tc.status, EndBody: true}
				}
				return musetest.Response{Reset: &musetest.Reset{Code: 1}}
			})
			session := newLiveChatSessionForTest(t, ws.URL)
			env := newChatTestEnv(t)
			view := env.start(t, &liveChatGateway{sess: session}, ChatStartRequest{})
			got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting, ChatStatusDeliveryUnknown, ChatStatusFailed)
			if got.Status == ChatStatusWaiting {
				if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
					t.Fatal(err)
				}
			}
			waitChatDone(t, env.svc, view.ChatID)
			if got.Status != tc.wantStatus || (got.UserSeq > 0) != (tc.wantStatus == ChatStatusWaiting) ||
				(tc.wantCode != "" && (got.Error == nil || got.Error.Code != tc.wantCode)) {
				t.Fatalf("unexpected snapshot: %+v", got)
			}
			writes, histories := 0, 0
			for _, req := range ws.Requests() {
				if req.Path == "/chat/stream" {
					writes++
				}
				if strings.HasPrefix(req.Path, "/chat/history?") {
					histories++
				}
			}
			if writes != 1 || (tc.wantStatus == ChatStatusFailed && histories != 1) || (!tc.disconnect && tc.wantStatus != ChatStatusFailed && histories <= 1) {
				t.Fatalf("writes=%d history reads=%d", writes, histories)
			}
			sent, err := env.st.ListAudit("", "muse.chat.sent", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantStatus == ChatStatusWaiting {
				wantEcho := "echo=no"
				if tc.status != 0 {
					wantEcho = "echo=yes"
				}
				if len(sent) != 1 || !strings.Contains(sent[0].Detail, wantEcho) || !strings.Contains(sent[0].Detail, "delivery=confirmed") {
					t.Fatalf("confirmed audit: %+v", sent)
				}
				for _, want := range []string{
					"send_status=" + fmt.Sprint(tc.status),
					"send_addr=implicit", "resolved=",
				} {
					if !strings.Contains(sent[0].Detail, want) {
						t.Fatalf("confirmed audit %q missing %q", sent[0].Detail, want)
					}
				}
			} else if len(sent) != 0 {
				t.Fatal("audited delivery without confirmation")
			}

			failed, err := env.st.ListAudit("", "muse.chat.failed", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			unknown, err := env.st.ListAudit("", "muse.chat.delivery_unknown", 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			switch tc.wantStatus {
			case ChatStatusFailed:
				if len(failed) != 1 || len(unknown) != 0 {
					t.Fatalf("failed audit rows=%d unknown rows=%d", len(failed), len(unknown))
				}
				// A definite 4xx must carry its status instead of silently
				// turning into delivery_unknown.
				if tc.status != 0 && !strings.Contains(failed[0].Detail, "status="+fmt.Sprint(tc.status)) {
					t.Fatalf("failed audit %q missing status=%d", failed[0].Detail, tc.status)
				}
			case ChatStatusDeliveryUnknown:
				if len(unknown) != 1 || len(failed) != 0 {
					t.Fatalf("unknown audit rows=%d failed rows=%d", len(unknown), len(failed))
				}
				for _, want := range []string{
					"send_status=" + fmt.Sprint(tc.status),
					"send_addr=implicit", "confirm_reads=", "confirm_errors=",
					"last_confirm_err=", "resolved=",
				} {
					if !strings.Contains(unknown[0].Detail, want) {
						t.Fatalf("unknown audit %q missing %q", unknown[0].Detail, want)
					}
				}
			}
		})
	}
}

// TestChatSideChatKeepsExplicitAddress pins the side-chat half of 4.1: both
// /chat/stream and /chat/history carry the caller's explicit id, the job
// reaches waiting, and the audit says send_addr=explicit.
func TestChatSideChatKeepsExplicitAddress(t *testing.T) {
	m := newMainChatUpstream()
	m.mu.Lock()
	m.addEventLocked("side-1", "message.user", "old side prompt")
	m.mu.Unlock()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	env := newChatTestEnv(t)
	view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)},
		ChatStartRequest{SessionID: "side-1", Text: "side text"})

	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	if got.BaselineSeq != 1 || got.UserSeq != got.BaselineSeq+1 || got.ConfirmedAt == 0 {
		t.Fatalf("snapshot: %+v", got)
	}
	if _, mainExplicit, implicit, side, lastSID := m.counts(); mainExplicit != 0 || implicit != 0 || side != 1 || lastSID != "side-1" {
		t.Fatalf("side address: explicit-main=%d implicit=%d side=%d last=%q",
			mainExplicit, implicit, side, lastSID)
	}
	if hadSessionID, _ := m.lastSendAddress(); !hadSessionID {
		t.Fatal("/chat/stream must carry session_id for a side chat")
	}
	for _, req := range ws.Requests() {
		if !strings.HasPrefix(req.Path, "/chat/history?") {
			continue
		}
		u, err := url.Parse(req.Path)
		if err != nil {
			t.Fatalf("history path: %v", err)
		}
		if got := u.Query().Get("session_id"); got != "side-1" {
			t.Fatalf("/chat/history session_id=%q, want side-1", got)
		}
	}
	sending, err := env.st.ListAudit("", "muse.chat.sending", 0, 20)
	if err != nil || len(sending) != 1 || !strings.Contains(sending[0].Detail, "send_addr=explicit") {
		t.Fatalf("sending audit: %+v err=%v", sending, err)
	}
	sent, err := env.st.ListAudit("", "muse.chat.sent", 0, 20)
	if err != nil || len(sent) != 1 {
		t.Fatalf("sent audit rows=%d err=%v", len(sent), err)
	}
	for _, want := range []string{"send_addr=explicit", "send_status=200", "resolved="} {
		if !strings.Contains(sent[0].Detail, want) {
			t.Fatalf("sent audit %q missing %q", sent[0].Detail, want)
		}
	}

	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatal(err)
	}
	waitChatDone(t, env.svc, view.ChatID)
}

// TestChatConfirmReadFailureIsAuditable pins 4.2: when the write is accepted
// and lands but every confirmation read fails, delivery_unknown must carry the
// send status and confirm_reads/confirm_errors/last_confirm_err instead of
// looking identical to "read succeeded, no event".
func TestChatConfirmReadFailureIsAuditable(t *testing.T) {
	oldBudget, oldEvery := chatConfirmBudget, chatConfirmEvery
	chatConfirmBudget, chatConfirmEvery = 250*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { chatConfirmBudget, chatConfirmEvery = oldBudget, oldEvery })

	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(func(req musetest.Request, emit *musetest.Emitter) musetest.Response {
		u, _ := url.Parse(req.Path)
		if req.Verb == http.MethodGet && u.Path == "/chat/history" && u.Query().Get("limit") == "20" {
			return musetest.Response{Status: 503, EndBody: true}
		}
		return m.handle(req, emit)
	})
	env := newChatTestEnv(t)
	view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)},
		ChatStartRequest{Text: "pwd"})

	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusDeliveryUnknown)
	waitChatDone(t, env.svc, view.ChatID)
	if got.Error == nil || got.Error.Code != "muse_delivery_unknown" {
		t.Fatalf("snapshot error: %+v", got.Error)
	}
	rows, err := env.st.ListAudit("", "muse.chat.delivery_unknown", 0, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("delivery_unknown rows=%d err=%v", len(rows), err)
	}
	detail := rows[0].Detail
	for _, want := range []string{
		"send_status=200", "send_echo=yes", "send_addr=implicit",
		"confirm_reads=", "confirm_errors=", "last_confirm_err=", "resolved=",
	} {
		if !strings.Contains(detail, want) {
			t.Fatalf("delivery_unknown audit %q missing %q", detail, want)
		}
	}
	reads, err := strconv.Atoi(chatAuditField(t, detail, "confirm_reads"))
	if err != nil || reads == 0 {
		t.Fatalf("confirm_reads=%q err=%v", chatAuditField(t, detail, "confirm_reads"), err)
	}
	errorsCount, err := strconv.Atoi(chatAuditField(t, detail, "confirm_errors"))
	if err != nil || errorsCount == 0 {
		t.Fatalf("confirm_errors=%q err=%v", chatAuditField(t, detail, "confirm_errors"), err)
	}
	if last := chatAuditField(t, detail, "last_confirm_err"); last == "none" {
		t.Fatalf("last_confirm_err=%q, want a classified read failure", last)
	}
	if fp := chatAuditField(t, detail, "resolved"); len(fp) != 8 {
		t.Fatalf("resolved=%q, want 8-hex fingerprint", fp)
	}

	histories := 0
	for _, req := range ws.Requests() {
		if strings.HasPrefix(req.Path, "/chat/history?") {
			histories++
		}
	}
	if histories < 2 {
		t.Fatalf("history reads=%d, want baseline plus failed confirmations", histories)
	}
}

func TestChatCancelDuringSendOrConfirmation(t *testing.T) {
	for _, phase := range []string{"sending", "confirming"} {
		t.Run(phase, func(t *testing.T) {
			upstream := newMainChatUpstream()
			ws := musetest.New(t)
			blocked := make(chan struct{})
			ws.SetStreamHandler(func(req musetest.Request, emit *musetest.Emitter) musetest.Response {
				if (phase == "sending" && req.Path == "/chat/stream") || (phase == "confirming" && strings.HasPrefix(req.Path, "/chat/history?") && strings.Contains(req.Path, "limit=20")) {
					close(blocked)
					return musetest.Response{Status: 200, NoEnd: true}
				}
				return upstream.handle(req, emit)
			})
			env := newChatTestEnv(t)
			view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)}, ChatStartRequest{})
			select {
			case <-blocked:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach " + phase)
			}
			if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
				t.Fatal(err)
			}
			waitChatDone(t, env.svc, view.ChatID)
			got, err := env.svc.GetChat(view.ChatID)
			if err != nil || got.Status != ChatStatusCancelled || got.ConfirmedAt != 0 {
				t.Fatalf("after cancel: %+v, %v", got, err)
			}
			writes, histories := 0, 0
			for _, req := range ws.Requests() {
				if req.Path == "/chat/stream" {
					writes++
				}
				if strings.HasPrefix(req.Path, "/chat/history?") {
					histories++
				}
			}
			wantHistories := 1
			if phase == "confirming" {
				wantHistories++
			}
			if writes != 1 || histories != wantHistories {
				t.Fatalf("writes=%d histories=%d", writes, histories)
			}
		})
	}
}

func (g *liveChatGateway) Open(context.Context, string) (chatSession, error) {
	return g.sess, nil
}

func TestChatEmptySessionReachesWaitingAndAuditsConfirmed(t *testing.T) {
	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	env := newChatTestEnv(t)
	view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)},
		ChatStartRequest{Text: "pwd"})

	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	if got.BaselineSeq != 0 || got.UserSeq != got.BaselineSeq+1 || got.ConfirmedAt == 0 {
		t.Fatalf("snapshot: %+v", got)
	}
	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	sending, err := env.st.ListAudit("", "muse.chat.sending", 0, 20)
	if err != nil || len(sending) != 1 {
		t.Fatalf("sending audit rows=%d err=%v", len(sending), err)
	}
	if !strings.Contains(sending[0].Detail, "send_addr=implicit") {
		t.Fatalf("sending audit detail: %q", sending[0].Detail)
	}
	rows, err := env.st.ListAudit("", "muse.chat.sent", 0, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("sent audit rows=%d err=%v", len(rows), err)
	}
	for _, want := range []string{
		"delivery=confirmed", "send_status=200", "send_echo=yes",
		"send_addr=implicit", "resolved=",
	} {
		if !strings.Contains(rows[0].Detail, want) {
			t.Fatalf("sent audit detail %q missing %q", rows[0].Detail, want)
		}
	}
	if fp := chatAuditField(t, rows[0].Detail, "resolved"); len(fp) != 8 {
		t.Fatalf("sent audit resolved=%q, want 8-hex fingerprint", fp)
	}

	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	if _, mainExplicit, implicit, _, _ := m.counts(); mainExplicit != 0 || implicit != 1 {
		t.Fatalf("orchestrator address: explicit-main=%d implicit=%d", mainExplicit, implicit)
	}
}

// TestChatSessionListEnvelopeResolvesMainChatToWaiting pins both documented
// sessions.list envelopes end to end: a bare {"sessions":[...]} response and a
// gateway-wrapped {"result":{"sessions":[...]}} response must resolve the main
// chat, write it implicitly, read it explicitly, reach waiting and audit
// delivery=confirmed.
func TestChatSessionListEnvelopeResolvesMainChatToWaiting(t *testing.T) {
	const (
		mainSecret = "main-session-secret"
		sideSecret = "side-session-secret"
		bodySecret = "upstream-body-secret"
	)
	cases := []struct {
		name string
		body string
	}{
		{
			name: "bare",
			body: `{"sessions":[` +
				`{"session_id":"` + mainSecret + `","title":"` + bodySecret + `","is_primary":true,"is_thread":false},` +
				`{"session_id":"` + sideSecret + `","title":"` + bodySecret + `","is_primary":false,"is_thread":true}]}`,
		},
		{
			name: "result_wrapped",
			body: `{"result":{"sessions":[` +
				`{"session_id":"` + mainSecret + `","title":"` + bodySecret + `","is_primary":true,"is_thread":false},` +
				`{"session_id":"` + sideSecret + `","title":"` + bodySecret + `","is_primary":false,"is_thread":true}]},` +
				`"meta":{"note":"` + bodySecret + `"}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureChatLog(t)
			m := newMainChatUpstream()
			m.mainID = mainSecret
			m.listBody = []byte(tc.body)
			ws := musetest.New(t)
			ws.SetStreamHandler(m.handle)
			env := newChatTestEnv(t)
			view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)},
				ChatStartRequest{Text: "pwd"})

			got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
			if got.BaselineSeq != 0 || got.UserSeq == 0 || got.ConfirmedAt == 0 {
				t.Fatalf("snapshot: %+v", got)
			}
			if got.Error != nil {
				t.Fatalf("unexpected error: %+v", got.Error)
			}
			sent, err := env.st.ListAudit("", "muse.chat.sent", 0, 20)
			if err != nil || len(sent) != 1 {
				t.Fatalf("sent audit rows=%d err=%v", len(sent), err)
			}
			if !strings.Contains(sent[0].Detail, "delivery=confirmed") {
				t.Fatalf("sent audit detail: %q", sent[0].Detail)
			}

			if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			waitChatDone(t, env.svc, view.ChatID)
			if _, mainExplicit, implicit, _, lastSID := m.counts(); mainExplicit != 0 || implicit != 1 || lastSID != "" {
				t.Fatalf("orchestrator address: explicit-main=%d implicit=%d last=%q",
					mainExplicit, implicit, lastSID)
			}
			if hadSessionID, _ := m.lastSendAddress(); hadSessionID {
				t.Fatalf("/chat/stream must omit session_id for the main chat")
			}

			all, err := env.st.ListAudit("", "", 0, 100)
			if err != nil {
				t.Fatalf("list audit: %v", err)
			}
			out := logs.String()
			for _, secret := range []string{mainSecret, sideSecret, bodySecret} {
				if strings.Contains(out, secret) {
					t.Fatalf("log leaked %q:\n%s", secret, out)
				}
				for _, row := range all {
					if strings.Contains(row.Detail, secret) {
						t.Fatalf("audit %s leaked %q: %q", row.Action, secret, row.Detail)
					}
				}
			}
			if strings.Contains(out, "session list failed") {
				t.Fatalf("session list unexpectedly failed:\n%s", out)
			}
			if strings.Contains(out, "top_keys=") {
				t.Fatalf("top_keys logged for a non-empty list:\n%s", out)
			}
		})
	}
}

// TestChatSessionListEnvelopeUnresolvedShapes pins the diagnostic summary for
// wrapped-but-side-only and empty-object responses. The shape carries counts
// and presence flags only; top_keys carries key names only, never values.
func TestChatSessionListEnvelopeUnresolvedShapes(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantShape []string
		secrets   []string
	}{
		{
			name: "result_wrapped_side_only",
			body: `{"result":{"sessions":[` +
				`{"session_id":"side-secret-id","title":"upstream-body-secret","is_primary":false,"is_thread":true}]},` +
				`"meta":{"note":"upstream-body-secret"}}`,
			wantShape: []string{
				"sessions=1", "primary_field=1", "thread_field=1", "entries=pT", "resolved=no",
			},
			secrets: []string{"side-secret-id", "upstream-body-secret"},
		},
		{
			name:      "empty_object",
			body:      `{}`,
			wantShape: []string{"sessions=0", "primary_field=0", "thread_field=0", "entries=none", "resolved=no", "top_keys=[]"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureChatLog(t)
			ws := musetest.New(t)
			streams := 0
			ws.SetStreamHandler(func(req musetest.Request, _ *musetest.Emitter) musetest.Response {
				switch {
				case req.Verb == http.MethodGet && req.Path == chatSessionsPath:
					return musetest.Response{Status: 200, Body: []byte(tc.body), EndBody: true}
				case req.Verb == http.MethodPost && req.Path == "/chat/stream":
					streams++
					return musetest.Response{Status: 200, Body: []byte("{}\n"), EndBody: true}
				default:
					return musetest.Response{Status: 404, EndBody: true}
				}
			})
			env := newChatTestEnv(t)
			view := env.start(t, &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)},
				ChatStartRequest{Text: "pwd"})
			got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
			waitChatDone(t, env.svc, view.ChatID)

			if got.Error == nil || got.Error.Code != "muse_history_unavailable" {
				t.Fatalf("public error changed: %+v", got.Error)
			}
			if streams != 0 {
				t.Fatalf("wrote a stream without a main-chat id: %d", streams)
			}
			rows, err := env.st.ListAudit("", "muse.chat.failed", 0, 20)
			if err != nil || len(rows) != 1 {
				t.Fatalf("failed audit rows=%d err=%v", len(rows), err)
			}
			detail := rows[0].Detail
			for _, want := range []string{"reason=muse_main_chat_unresolved", "error_class=muse_history_unavailable"} {
				if !strings.Contains(detail, want) {
					t.Fatalf("audit detail %q missing %q", detail, want)
				}
			}
			if strings.Contains(detail, "status=") {
				t.Fatalf("2xx session-list failure carried status=: %q", detail)
			}

			out := logs.String()
			for _, want := range append([]string{
				"muse chat: session list failed", "stage=session_list", "reason=muse_main_chat_unresolved",
			}, tc.wantShape...) {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			for _, secret := range tc.secrets {
				if strings.Contains(detail, secret) || strings.Contains(out, secret) {
					t.Fatalf("secret/body leaked: audit=%q logs=%q", detail, out)
				}
			}
		})
	}
}

package muse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

type fakeChatSession struct {
	mu sync.Mutex

	historyFn func(call, limit int, before int64) (*chatHistoryResponse, error)
	sendFn    func(text string) (chatSendResult, error)
	cancelFn  func(sessionID string) (chatCancelResult, error)

	historyCalls int
	sendCalls    int
	cancelCalls  int
	sentText     string
	closed       bool
}

func (f *fakeChatSession) History(_ context.Context, _ string, limit int, before int64) (*chatHistoryResponse, error) {
	f.mu.Lock()
	f.historyCalls++
	call := f.historyCalls
	f.mu.Unlock()
	if f.historyFn == nil {
		return &chatHistoryResponse{}, nil
	}
	return f.historyFn(call, limit, before)
}

func (f *fakeChatSession) Send(_ context.Context, _, text, _ string) (chatSendResult, error) {
	f.mu.Lock()
	f.sendCalls++
	f.sentText = text
	f.mu.Unlock()
	if f.sendFn != nil {
		return f.sendFn(text)
	}
	return chatSendResult{Wrote: true, Status: 200, Echo: true}, nil
}

func (f *fakeChatSession) Cancel(_ context.Context, sessionID string) (chatCancelResult, error) {
	f.mu.Lock()
	f.cancelCalls++
	f.mu.Unlock()
	if f.cancelFn != nil {
		return f.cancelFn(sessionID)
	}
	return chatCancelResult{Sent: true, Status: 200}, nil
}

func (f *fakeChatSession) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

func (f *fakeChatSession) counts() (history, send, cancel int, sent string, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.historyCalls, f.sendCalls, f.cancelCalls, f.sentText, f.closed
}

type fakeChatGateway struct {
	mu       sync.Mutex
	openErr  error
	factory  func(open int) *fakeChatSession
	sessions []*fakeChatSession
	opens    int
}

func (g *fakeChatGateway) Open(_ context.Context, _ string) (chatSession, error) {
	g.mu.Lock()
	g.opens++
	open := g.opens
	err := g.openErr
	factory := g.factory
	var fixed *fakeChatSession
	if len(g.sessions) > 0 {
		fixed = g.sessions[0]
	}
	g.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if factory != nil {
		return factory(open), nil
	}
	if fixed != nil {
		return fixed, nil
	}
	return &fakeChatSession{}, nil
}

func (g *fakeChatGateway) openCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.opens
}

func chatUserEvent(seq int64, text string) chatEvent {
	return chatEvent{
		EventName: "message.user", Seq: seq,
		MessageID: fmt.Sprintf("user-%d", seq),
		Payload:   chatPayload{DisplayText: text, SessionID: ""},
	}
}

func chatAssistantEvent(seq int64, text, status string) chatEvent {
	return chatEvent{
		EventName: "message.assistant", Seq: seq,
		MessageID: fmt.Sprintf("assistant-%d", seq),
		Payload:   chatPayload{DisplayText: text, Status: status, SessionID: ""},
	}
}

type chatTestEnv struct {
	*testEnv
	account *Account
}

func newChatTestEnv(t *testing.T) *chatTestEnv {
	t.Helper()
	env := newTestEnv(t, false)
	env.svc.opts.DirectChatEnabled = true
	env.svc.chatRuntime = true
	env.svc.opts.ChatPollInterval = 25 * time.Millisecond
	env.svc.opts.ChatDefaultBudget = 30 * time.Second
	env.svc.opts.ChatMaxBudget = 300 * time.Second
	acct := env.authorize(t, "alice")
	if _, err := env.svc.SetChatConsent(acct.ID, true, 1, "member said yes", "", "admin"); err != nil {
		t.Fatalf("consent: %v", err)
	}
	return &chatTestEnv{testEnv: env, account: acct}
}

func (e *chatTestEnv) start(t *testing.T, gw *fakeChatGateway, req ChatStartRequest) *ChatJobView {
	t.Helper()
	if e.svc.chatOpen == nil {
		e.svc.chatOpen = gw
	}
	if req.PeerID == "" {
		req.PeerID = "alice"
	}
	if req.Text == "" {
		req.Text = "hello muse"
	}
	v, err := e.svc.StartChat(context.Background(), req)
	if err != nil {
		t.Fatalf("StartChat: %v", err)
	}
	return v
}

func waitChatStatus(t *testing.T, svc *Service, id string, want ...string) *ChatJobView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		v, err := svc.GetChat(id)
		if err == nil {
			for _, w := range want {
				if v.Status == w {
					return v
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	v, _ := svc.GetChat(id)
	t.Fatalf("chat %s did not reach %v; last=%+v", id, want, v)
	return nil
}

func waitChatPolls(t *testing.T, svc *Service, id string, min int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := svc.GetChat(id); err == nil && v.PollCount >= min {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("chat %s did not reach %d poll(s)", id, min)
}

func waitChatDone(t *testing.T, svc *Service, id string) {
	t.Helper()
	svc.chatMu.Lock()
	job := svc.chatJobs[id]
	svc.chatMu.Unlock()
	if job == nil {
		return
	}
	select {
	case <-job.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("chat %s worker did not stop", id)
	}
}

func chatAdmissionCode(t *testing.T, err error) (*ChatAdmissionError, string) {
	t.Helper()
	var ce *ChatAdmissionError
	if !errors.As(err, &ce) {
		t.Fatalf("not an admission error: %v", err)
	}
	return ce, ce.Code
}

func TestChatHappyPathPollsReplyAndAuditsMetadataOnly(t *testing.T) {
	env := newChatTestEnv(t)
	gw := &fakeChatGateway{factory: func(int) *fakeChatSession {
		return &fakeChatSession{historyFn: func(call, limit int, _ int64) (*chatHistoryResponse, error) {
			switch call {
			case 1:
				return &chatHistoryResponse{ChatEvents: []chatEvent{
					chatAssistantEvent(10, "old answer", "completed"),
				}}, nil
			default:
				return &chatHistoryResponse{ChatEvents: []chatEvent{
					chatAssistantEvent(10, "old answer", "completed"),
					chatUserEvent(11, "hello muse"),
					chatAssistantEvent(12, "the answer", "completed"),
				}}, nil
			}
		}}
	}}
	view := env.start(t, gw, ChatStartRequest{Text: "hello muse"})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusReplied)
	if got.Reply == nil || got.Reply.Text != "the answer" || got.Reply.Seq != 12 {
		t.Fatalf("reply: %+v", got.Reply)
	}
	if got.Confidence != "high" || got.BaselineSeq != 10 || got.UserSeq != 11 {
		t.Fatalf("snapshot: %+v", got)
	}
	if got.SentAt == 0 || got.ConfirmedAt == 0 || got.DeadlineAt <= got.ConfirmedAt {
		t.Fatalf("timestamps: %+v", got)
	}
	for _, action := range []string{"muse.chat.sending", "muse.chat.sent", "muse.chat.replied"} {
		rows, err := env.st.ListAudit("", action, 0, 20)
		if err != nil || len(rows) != 1 {
			t.Fatalf("%s audit rows=%d err=%v", action, len(rows), err)
		}
		if strings.Contains(rows[0].Detail, "hello muse") || strings.Contains(rows[0].Detail, "the answer") {
			t.Fatalf("%s leaked content: %q", action, rows[0].Detail)
		}
	}
	acct, _ := env.svc.Get(env.account.ID)
	if acct.LastChatStatus != ChatStatusReplied {
		t.Fatalf("last_chat_status: %q", acct.LastChatStatus)
	}
}

func TestChatBaselineFailureNeverSends(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		return nil, &chatFailure{Code: "muse_upstream_timeout", Stage: "history", Retryable: true}
	}}
	gw := &fakeChatGateway{sessions: []*fakeChatSession{sess}}
	view := env.start(t, gw, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
	if got.Error == nil || got.Error.Code != "muse_history_unavailable" {
		t.Fatalf("error: %+v", got.Error)
	}
	_, sends, _, _, _ := sess.counts()
	if sends != 0 {
		t.Fatalf("sent despite baseline failure: %d", sends)
	}
	rows, _ := env.st.ListAudit("", "muse.chat.sending", 0, 20)
	if len(rows) != 0 {
		t.Fatalf("sending audit written without send: %+v", rows)
	}
}

func TestChatBusyDraftBlocksSend(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatUserEvent(9, "older"),
			chatAssistantEvent(10, "", ""),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
	if got.Error == nil || got.Error.Code != "muse_agent_busy" {
		t.Fatalf("error: %+v", got.Error)
	}
	_, sends, _, _, _ := sess.counts()
	if sends != 0 {
		t.Fatalf("sent while agent busy: %d", sends)
	}
}

func TestChatDeliveryUnknownDoesNotResend(t *testing.T) {
	oldBudget, oldEvery := chatConfirmBudget, chatConfirmEvery
	chatConfirmBudget, chatConfirmEvery = 100*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { chatConfirmBudget, chatConfirmEvery = oldBudget, oldEvery })

	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusDeliveryUnknown)
	if got.Error == nil || got.Error.Code != "muse_delivery_unknown" {
		t.Fatalf("error: %+v", got.Error)
	}
	_, sends, _, _, _ := sess.counts()
	if sends != 1 {
		t.Fatalf("automatic resend: sends=%d", sends)
	}
}

func TestChatTimeoutAndDraftPartial(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		switch call {
		case 1:
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		case 2:
			return &chatHistoryResponse{ChatEvents: []chatEvent{
				chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
			}}, nil
		default:
			return &chatHistoryResponse{ChatEvents: []chatEvent{
				chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
				chatAssistantEvent(12, "still writing", ""),
			}}, nil
		}
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	waitChatPolls(t, env.svc, view.ChatID, 1)
	env.clock += 31
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusTimeout)
	if got.Error == nil || got.Error.Code != "muse_reply_timeout" {
		t.Fatalf("error: %+v", got.Error)
	}
	if got.PartialText != "still writing" || !got.PartialDraft {
		t.Fatalf("partial: %+v", got)
	}
	rows, _ := env.st.ListAudit("", "muse.chat.timeout", 0, 20)
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "partial=yes") {
		t.Fatalf("timeout audit: %+v", rows)
	}
}

func TestChatCancelSendsCancelAndStopsPolling(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	got, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin")
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if got.Status != ChatStatusCancelled || got.Cancel != "sent" {
		t.Fatalf("cancel snapshot: %+v", got)
	}
	waitChatDone(t, env.svc, view.ChatID)
	_, _, cancels, _, _ := sess.counts()
	if cancels != 1 {
		t.Fatalf("cancel RPC count: %d", cancels)
	}
	rows, _ := env.st.ListAudit("", "muse.chat.cancelled", 0, 20)
	if len(rows) != 1 || !strings.Contains(rows[0].Detail, "cancel=sent") || rows[0].Actor != "admin" {
		t.Fatalf("cancel audit: %+v", rows)
	}
	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); !errors.Is(err, ErrChatNotActive) {
		t.Fatalf("second cancel: %v", err)
	}
}

func TestChatHistoryResetFails(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		switch call {
		case 1:
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		case 2:
			return &chatHistoryResponse{ChatEvents: []chatEvent{
				chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
			}}, nil
		default:
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(5, "rewound", "completed")}}, nil
		}
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
	if got.Error == nil || got.Error.Code != "muse_history_reset" {
		t.Fatalf("error: %+v", got.Error)
	}
}

func TestChatAdmissionGatesAndIdempotency(t *testing.T) {
	env := newChatTestEnv(t)
	gw := &fakeChatGateway{sessions: []*fakeChatSession{{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}}}
	view := env.start(t, gw, ChatStartRequest{ClientRequestID: "req-1"})
	dup := env.start(t, gw, ChatStartRequest{ClientRequestID: "req-1"})
	if !dup.Duplicate || dup.ChatID != view.ChatID {
		t.Fatalf("duplicate: %+v", dup)
	}
	_, err := env.svc.StartChat(context.Background(), ChatStartRequest{
		PeerID: "alice", Text: "different text", ClientRequestID: "req-1",
	})
	_, code := chatAdmissionCode(t, err)
	if code != "idempotency_conflict" {
		t.Fatalf("idempotency conflict code: %s", code)
	}
	_, err = env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "alice", Text: "second"})
	ce, code := chatAdmissionCode(t, err)
	if code != "muse_chat_in_progress" || ce.JobID != view.ChatID {
		t.Fatalf("in progress: %+v", ce)
	}
	_, err = env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "alice", Text: "   "})
	_, code = chatAdmissionCode(t, err)
	if code != "text_empty" {
		t.Fatalf("empty text: %s", code)
	}
	long := strings.Repeat("字", 4001)
	_, err = env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "alice", Text: long})
	_, code = chatAdmissionCode(t, err)
	if code != "text_too_long" {
		t.Fatalf("long text: %s", code)
	}
	env.svc.chatRuntime = false
	_, err = env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "alice", Text: "off"})
	ce, code = chatAdmissionCode(t, err)
	if ce.Status != 503 || code != "muse_chat_disabled" {
		t.Fatalf("disabled: %+v", ce)
	}
}

func TestChatRateLimitAndGlobalCapacity(t *testing.T) {
	env := newChatTestEnv(t)
	env.svc.chatSem = make(chan struct{}, 1)
	env.svc.opts.ChatPerPeerPerMin = 1
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)

	bob := env.authorize(t, "bob")
	if _, err := env.svc.SetChatConsent(bob.ID, true, 1, "bob said yes", "", "admin"); err != nil {
		t.Fatalf("bob consent: %v", err)
	}
	_, err := env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "bob", Text: "hi bob"})
	ce, code := chatAdmissionCode(t, err)
	if ce.Status != 429 || code != "muse_chat_busy" || ce.RetryAfterSecs != 5 {
		t.Fatalf("global busy: %+v", ce)
	}
	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	_, err = env.svc.StartChat(context.Background(), ChatStartRequest{PeerID: "alice", Text: "again"})
	ce, code = chatAdmissionCode(t, err)
	if ce.Status != 429 || code != "muse_chat_rate_limited" || ce.RetryAfterSecs < 1 {
		t.Fatalf("rate: %+v", ce)
	}
}

func TestChatAuditFailureBlocksSend(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
	}}
	env.svc.st = failAuditStore{Store: env.st}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
	if got.Error == nil || got.Error.Code != "muse_audit_unavailable" {
		t.Fatalf("error: %+v", got.Error)
	}
	_, sends, _, _, _ := sess.counts()
	if sends != 0 {
		t.Fatalf("sent without a sending audit row: %d", sends)
	}
}

func TestChatRuntimeKillSwitchCancelsWithoutRPC(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	status, err := env.svc.SetChatRuntime(false, "admin")
	if err != nil || status.RuntimeEnabled {
		t.Fatalf("disable: %+v %v", status, err)
	}
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusCancelled)
	waitChatDone(t, env.svc, view.ChatID)
	_, _, cancels, _, _ := sess.counts()
	if cancels != 0 {
		t.Fatalf("kill switch sent chat.cancel: %d", cancels)
	}
	status, err = env.svc.SetChatRuntime(true, "admin")
	if err != nil || !status.RuntimeEnabled {
		t.Fatalf("enable: %+v %v", status, err)
	}
	rows, _ := env.st.ListAudit("", "muse.chat.disabled", 0, 20)
	if len(rows) != 1 {
		t.Fatalf("disabled audit: %+v", rows)
	}
}

func TestChatLeaseBlocksKeepalive(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	if _, err := env.svc.Keepalive(context.Background(), env.account.ID); !errors.Is(err, ErrChatActive) {
		t.Fatalf("keepalive during chat: %v", err)
	}
	if _, err := env.svc.Wake(context.Background(), env.account.ID); !errors.Is(err, ErrChatActive) {
		t.Fatalf("wake during chat: %v", err)
	}
	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	if _, err := env.svc.Keepalive(context.Background(), env.account.ID); err != nil {
		t.Fatalf("keepalive after lease release: %v", err)
	}
}

func TestChatConsentResetOnPeerChangeAndReauthorize(t *testing.T) {
	env := newChatTestEnv(t)
	if a, _ := env.svc.Get(env.account.ID); !a.ChatEnabled {
		t.Fatal("consent was not enabled")
	}
	bobID := "bob"
	if _, err := env.svc.Update(env.account.ID, nil, &bobID, nil, nil); err != nil {
		t.Fatalf("update peer: %v", err)
	}
	if a, _ := env.svc.Get(env.account.ID); a.ChatEnabled {
		t.Fatal("peer change did not reset chat consent")
	}
	// Put the binding back and enable again, then reauthorize a new cookie.
	aliceID := "alice"
	if _, err := env.svc.Update(env.account.ID, nil, &aliceID, nil, nil); err != nil {
		t.Fatalf("restore peer: %v", err)
	}
	if _, err := env.svc.SetChatConsent(env.account.ID, true, 1, "member said yes again", "", "admin"); err != nil {
		t.Fatalf("consent again: %v", err)
	}
	if _, err := env.svc.Reauthorize(context.Background(), env.account.ID,
		"hatch_sess=new-sess; hatch_vml=new-vml; hatch_gw=gw"); err != nil {
		t.Fatalf("reauthorize: %v", err)
	}
	if a, _ := env.svc.Get(env.account.ID); a.ChatEnabled || a.ChatConsentVersion != 0 {
		t.Fatalf("reauthorize did not reset consent: %+v", a)
	}
}

func TestChatConsentValidation(t *testing.T) {
	env := newChatTestEnv(t)
	_, err := env.svc.SetChatConsent(env.account.ID, true, 1, "short", "", "admin")
	ce, code := chatAdmissionCode(t, err)
	if ce.Status != 400 || code != "consent_evidence_required" {
		t.Fatalf("evidence: %+v", ce)
	}
	_, err = env.svc.SetChatConsent(env.account.ID, true, 2, "long enough evidence", "", "admin")
	_, code = chatAdmissionCode(t, err)
	if code != "consent_version_mismatch" {
		t.Fatalf("version: %s", code)
	}
	if _, err := env.svc.Update(env.account.ID, nil, nil, nil, boolPtr(false)); err != nil {
		t.Fatalf("disable account: %v", err)
	}
	_, err = env.svc.SetChatConsent(env.account.ID, true, 1, "long enough evidence", "", "admin")
	_, code = chatAdmissionCode(t, err)
	if code != "muse_account_disabled" {
		t.Fatalf("disabled account: %s", code)
	}
}

func TestChatTerminalRetentionAndGC(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusWaiting)
	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	env.clock += int64(chatJobRetention/time.Second) - 5
	if _, err := env.svc.GetChat(view.ChatID); err != nil {
		t.Fatalf("job GC'd too early: %v", err)
	}
	env.clock += 10
	if _, err := env.svc.GetChat(view.ChatID); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("job not GC'd: %v", err)
	}
	rows, _ := env.st.ListAudit("", "muse.chat.cancelled", 0, 20)
	if len(rows) != 1 {
		t.Fatalf("audit must outlive job GC: %+v", rows)
	}
}

type failAuditStore struct {
	store.Store
}

func (f failAuditStore) AppendAudit(_, _, _ string, _ int64) error {
	return errors.New("audit db down")
}

func TestChatReplyUnknownAfterReconnect(t *testing.T) {
	env := newChatTestEnv(t)
	gw := &fakeChatGateway{factory: func(open int) *fakeChatSession {
		if open > 1 {
			return &fakeChatSession{historyFn: func(int, int, int64) (*chatHistoryResponse, error) {
				return nil, &chatFailure{Code: "muse_upstream_timeout", Stage: "history", Retryable: true}
			}}
		}
		return &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
			if call == 1 {
				return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
			}
			if call == 2 {
				return &chatHistoryResponse{ChatEvents: []chatEvent{
					chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "hello muse"),
				}}, nil
			}
			return nil, &chatFailure{Code: "muse_upstream_timeout", Stage: "history", Retryable: true}
		}}
	}}
	view := env.start(t, gw, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusReplyUnknown)
	if got.Error == nil || got.Error.Code != "muse_reply_unknown" {
		t.Fatalf("error: %+v", got.Error)
	}
	if gw.openCount() < 2 {
		t.Fatalf("no read-only reconnect: %d", gw.openCount())
	}
}

func TestChatStaleUserDoesNotBlock(t *testing.T) {
	env := newChatTestEnv(t)
	nowMS := env.clock * 1000
	gw := &fakeChatGateway{factory: func(int) *fakeChatSession {
		return &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
			stale := chatEvent{EventName: "message.user", Seq: 10, OccurredAtMS: nowMS - int64(11*time.Minute/time.Millisecond),
				Payload: chatPayload{DisplayText: "stale question"}}
			old := chatAssistantEvent(9, "old", "completed")
			if call == 1 {
				return &chatHistoryResponse{ChatEvents: []chatEvent{old, stale}}, nil
			}
			ours := chatUserEvent(11, "hello muse")
			ours.OccurredAtMS = nowMS
			return &chatHistoryResponse{ChatEvents: []chatEvent{old, stale, ours, chatAssistantEvent(12, "fresh answer", "completed")}}, nil
		}}
	}}
	view := env.start(t, gw, ChatStartRequest{})
	got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusReplied)
	if got.Reply == nil || got.Reply.Text != "fresh answer" {
		t.Fatalf("reply: %+v", got)
	}
}

func TestChatDeployGateBlocksRuntimeEnable(t *testing.T) {
	env := newChatTestEnv(t)
	env.svc.opts.DirectChatEnabled = false
	_, err := env.svc.SetChatRuntime(true, "admin")
	ce, code := chatAdmissionCode(t, err)
	if ce.Status != 409 || code != "deploy_flag_off" {
		t.Fatalf("deploy gate: %+v", ce)
	}
}

package muse

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
	"github.com/AlixWang/agent-relay/internal/store"
)

func newNudgeTestEnv(t *testing.T) *chatTestEnv {
	t.Helper()
	env := newChatTestEnv(t)
	env.svc.opts.TaskNudgeEnabled = true
	env.svc.opts.TaskNudgeAfter = 180 * time.Second
	env.svc.opts.TaskNudgeCooldown = 600 * time.Second
	return env
}

func TestNudgeForTasksSuccessConfirmAndReleaseLease(t *testing.T) {
	env := newNudgeTestEnv(t)
	var sent string
	sess := &fakeChatSession{
		historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
			if call == 1 {
				return &chatHistoryResponse{
					ChatEvents: []chatEvent{
						chatAssistantEvent(10, "old reply", "completed"),
					},
				}, nil
			}
			return &chatHistoryResponse{
				ChatEvents: []chatEvent{
					chatAssistantEvent(10, "old reply", "completed"),
					chatUserEvent(11, sent),
				},
			}, nil
		},
		sendFn: func(text string) (chatSendResult, error) {
			sent = text
			return chatSendResult{Evidence: chatSendMayHaveArrived, Status: 200, Echo: true}, nil
		},
	}
	env.svc.chatOpen = &fakeChatGateway{sessions: []*fakeChatSession{sess}}

	res, err := env.svc.NudgeForTasks(context.Background(), "alice", []string{"op-task-1"})
	if err != nil {
		t.Fatalf("NudgeForTasks: %v", err)
	}
	if res.Status != "sent" || res.UserSeq != 11 {
		t.Fatalf("unexpected nudge result: %+v", res)
	}
	if !strings.Contains(sent, "op-task-1") {
		t.Fatalf("nudge text missing task id: %q", sent)
	}
	_, sendCalls, _, _, closed := sess.counts()
	if sendCalls != 1 || !closed {
		t.Fatalf("expected 1 send and closed session, got send=%d closed=%v", sendCalls, closed)
	}
	if env.svc.chatLeaseActive(env.account.ID) {
		t.Fatal("nudge leaked account lease after return")
	}
	audits, err := env.st.ListAudit("system", "muse.nudge.sent", 0, 10)
	if err != nil || len(audits) != 1 || !strings.Contains(audits[0].Detail, "op-task-1") {
		t.Fatalf("muse.nudge.sent audit missing: %+v err=%v", audits, err)
	}
}

// TestNudgeUsesImplicitMainChatSend pins the nudge reuse of Send(""): the
// write omits session_id, the confirmation read uses the resolved explicit id,
// and muse.nudge.sent still appears.
func TestNudgeUsesImplicitMainChatSend(t *testing.T) {
	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	env := newNudgeTestEnv(t)
	env.svc.chatOpen = &liveChatGateway{sess: newLiveChatSessionForTest(t, ws.URL)}

	res, err := env.svc.NudgeForTasks(context.Background(), "alice", []string{"op-nudge-1"})
	if err != nil {
		t.Fatalf("NudgeForTasks: %v", err)
	}
	if res.Status != "sent" || res.UserSeq != 1 {
		t.Fatalf("unexpected nudge result: %+v", res)
	}
	_, mainExplicit, implicit, side, lastSID := m.counts()
	if mainExplicit != 0 || implicit != 1 || side != 0 || lastSID != "" {
		t.Fatalf("nudge address: explicit-main=%d implicit=%d side=%d last=%q",
			mainExplicit, implicit, side, lastSID)
	}
	if hadSessionID, _ := m.lastSendAddress(); hadSessionID {
		t.Fatal("nudge /chat/stream must omit session_id for the main chat")
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
			t.Fatalf("nudge /chat/history session_id=%q, want explicit %q", got, m.mainID)
		}
	}
	audits, err := env.st.ListAudit("system", "muse.nudge.sent", 0, 10)
	if err != nil || len(audits) != 1 {
		t.Fatalf("muse.nudge.sent audit rows=%d err=%v", len(audits), err)
	}
}

func TestNudgeSkipsWhenAgentBusy(t *testing.T) {
	env := newNudgeTestEnv(t)
	now := env.clock
	sess := &fakeChatSession{
		historyFn: func(_, _ int, _ int64) (*chatHistoryResponse, error) {
			ev := chatUserEvent(20, "user prompt")
			ev.OccurredAtMS = (now - 10) * 1000 // unanswered user prompt 10s ago => busy
			return &chatHistoryResponse{ChatEvents: []chatEvent{ev}}, nil
		},
	}
	env.svc.chatOpen = &fakeChatGateway{sessions: []*fakeChatSession{sess}}

	res, err := env.svc.NudgeForTasks(context.Background(), "alice", []string{"op-task-2"})
	if err != nil {
		t.Fatalf("NudgeForTasks: %v", err)
	}
	if res.Status != "skipped" || res.Reason != "agent_busy" {
		t.Fatalf("expected agent_busy skip, got %+v", res)
	}
	_, sendCalls, _, _, closed := sess.counts()
	if sendCalls != 0 || !closed {
		t.Fatalf("busy agent must not receive send and session must close: send=%d closed=%v", sendCalls, closed)
	}
	audits, _ := env.st.ListAudit("system", "muse.nudge.skipped", 0, 10)
	if len(audits) != 1 || !strings.Contains(audits[0].Detail, "reason=agent_busy") {
		t.Fatalf("muse.nudge.skipped audit missing: %+v", audits)
	}
}

func TestNudgeSkipsWhenManualChatActive(t *testing.T) {
	env := newNudgeTestEnv(t)
	gw := &fakeChatGateway{}
	env.svc.chatOpen = gw

	env.svc.chatMu.Lock()
	env.svc.chatActive[env.account.ID] = "mc_manual"
	env.svc.chatMu.Unlock()
	defer func() {
		env.svc.chatMu.Lock()
		delete(env.svc.chatActive, env.account.ID)
		env.svc.chatMu.Unlock()
	}()

	res, err := env.svc.NudgeForTasks(context.Background(), "alice", []string{"op-task-3"})
	if err != nil {
		t.Fatalf("NudgeForTasks: %v", err)
	}
	if res.Status != "skipped" || res.Reason != "chat_active" {
		t.Fatalf("expected chat_active skip, got %+v", res)
	}
	if gw.openCount() != 0 {
		t.Fatalf("must not dial Noise when manual chat owns lease")
	}
}

func TestSweepStalledTasksOncePerTaskAndCooldown(t *testing.T) {
	env := newNudgeTestEnv(t)
	now := env.clock

	gw := &fakeChatGateway{
		factory: func(open int) *fakeChatSession {
			var sent string
			base := int64(open * 10)
			return &fakeChatSession{
				historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
					if call == 1 {
						return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(base+1, "prev", "completed")}}, nil
					}
					return &chatHistoryResponse{ChatEvents: []chatEvent{chatUserEvent(base+2, sent)}}, nil
				},
				sendFn: func(text string) (chatSendResult, error) {
					sent = text
					return chatSendResult{Evidence: chatSendMayHaveArrived, Status: 200, Echo: true}, nil
				},
			}
		},
	}
	env.svc.chatOpen = gw

	// Seed one stalled task (200s old, started only, no ack/progress/result).
	if _, err := env.st.InsertMessage(&store.Message{
		ID: "op-stalled-1", Sender: "operator", Recipient: "alice", Kind: "task",
		RootID: "op/alice/op-stalled-1", Payload: "run it", CreatedAt: now - 200, ApprovalState: "n/a",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.st.InsertMessage(&store.Message{
		ID: "st-stalled-1", Sender: "alice", Recipient: "operator", Kind: "status", Status: "started",
		InReplyTo: "op-stalled-1", RootID: "op/alice/op-stalled-1", Payload: "started", CreatedAt: now - 195, ApprovalState: "n/a",
	}); err != nil {
		t.Fatal(err)
	}

	env.svc.SweepStalledTasks(context.Background())

	// Wait for the async nudge goroutine to finish completely (avoid racing env.clock).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := env.st.ListAudit("system", "muse.nudge.sent", 0, 10)
		env.svc.mu.Lock()
		busy := env.svc.nudging["alice"]
		env.svc.mu.Unlock()
		if len(rows) == 1 && !busy {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if rows, _ := env.st.ListAudit("system", "muse.nudge.sent", 0, 10); len(rows) != 1 {
		t.Fatalf("expected 1 muse.nudge.sent row, got %+v", rows)
	}

	// A new stalled task within cooldown (+100s < 600s) is deferred by cooldown.
	if _, err := env.st.InsertMessage(&store.Message{
		ID: "op-stalled-2", Sender: "operator", Recipient: "alice", Kind: "task",
		RootID: "op/alice/op-stalled-2", Payload: "run 2", CreatedAt: env.clock - 200, ApprovalState: "n/a",
	}); err != nil {
		t.Fatal(err)
	}
	env.clock += 100
	env.svc.SweepStalledTasks(context.Background())
	time.Sleep(50 * time.Millisecond)
	if gw.openCount() != 1 {
		t.Fatalf("peer cooldown must suppress immediate second nudge, opens=%d", gw.openCount())
	}

	// After cooldown (+700s > 600s), op-stalled-1 is still skipped (nudged once) while op-stalled-2 is nudged once.
	env.clock += 700
	env.svc.SweepStalledTasks(context.Background())
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _ := env.st.ListAudit("system", "muse.nudge.sent", 0, 10)
		env.svc.mu.Lock()
		busy := env.svc.nudging["alice"]
		env.svc.mu.Unlock()
		if len(rows) == 2 && !busy {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if gw.openCount() != 2 {
		t.Fatalf("expected second nudge only for op-stalled-2 after cooldown, opens=%d", gw.openCount())
	}
	if rows, _ := env.st.ListAudit("system", "muse.nudge.sent", 0, 10); len(rows) != 2 || strings.Contains(rows[0].Detail, "op-stalled-1") && strings.Contains(rows[1].Detail, "op-stalled-1") {
		t.Fatalf("op-stalled-1 must not be nudged twice: %+v", rows)
	}
}

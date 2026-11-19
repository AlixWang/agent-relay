package muse

import (
	"strings"
	"testing"
	"time"
)

func TestChatThreadReturnsOnlyTheSelectedSession(t *testing.T) {
	env := newChatTestEnv(t)
	sess := &fakeChatSession{historyFn: func(call, _ int, _ int64) (*chatHistoryResponse, error) {
		if call == 1 {
			return &chatHistoryResponse{ChatEvents: []chatEvent{chatAssistantEvent(10, "old", "completed")}}, nil
		}
		return &chatHistoryResponse{ChatEvents: []chatEvent{
			chatAssistantEvent(10, "old", "completed"), chatUserEvent(11, "operator secret text"),
			chatAssistantEvent(12, "the answer", "completed"),
		}}, nil
	}}
	view := env.start(t, &fakeChatGateway{sessions: []*fakeChatSession{sess}}, ChatStartRequest{Text: "operator secret text"})
	waitChatStatus(t, env.svc, view.ChatID, ChatStatusReplied)

	// The job snapshot stays text-free.
	if _, err := env.svc.GetChat(view.ChatID); err != nil {
		t.Fatalf("get: %v", err)
	}

	thread, err := env.svc.ChatThread("alice", "")
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if thread.RetentionSecs != int(chatJobRetention/time.Second) || thread.PeerID != "alice" {
		t.Fatalf("thread metadata: %+v", thread)
	}
	if len(thread.Jobs) != 1 || thread.Jobs[0].ChatID != view.ChatID {
		t.Fatalf("main thread jobs: %+v", thread.Jobs)
	}
	job := thread.Jobs[0]
	if job.Text != "operator secret text" || job.Reply == nil || job.Reply.Text != "the answer" {
		t.Fatalf("thread text/reply: %+v", job)
	}
	if job.Status != ChatStatusReplied {
		t.Fatalf("thread status: %q", job.Status)
	}

	// A different session must not expose the main-chat turn.
	side, err := env.svc.ChatThread("alice", "s_side")
	if err != nil {
		t.Fatalf("side thread: %v", err)
	}
	if len(side.Jobs) != 0 {
		t.Fatalf("side thread leaked main chat: %+v", side.Jobs)
	}

	if _, err := env.svc.ChatThread("nobody", ""); err == nil {
		t.Fatal("unknown peer accepted")
	}
	if _, err := env.svc.ChatThread("alice", strings.Repeat("x", 129)); err == nil {
		t.Fatal("invalid session accepted")
	}
}

func TestChatThreadBoundaryAndRetention(t *testing.T) {
	env := newChatTestEnv(t)
	thread, err := env.svc.ChatThread("alice", "")
	if err != nil {
		t.Fatalf("empty thread: %v", err)
	}
	if thread.Jobs == nil || len(thread.Jobs) != 0 || thread.Sessions == nil {
		t.Fatalf("empty thread shape: %+v", thread)
	}
	env.clock += int64(chatJobRetention/time.Second) + 1
	// The job map GC only runs on access; ChatThread must invoke it and hide
	// everything older than the retention boundary.
	old := &ChatJob{ID: "mc_old", AccountID: env.account.ID, PeerID: "alice",
		SessionID: "", Session: "main", Text: "old text",
		CreatedAt: env.svc.now().Unix() - int64(chatJobRetention/time.Second) - 10,
		Status:    ChatStatusReplied, Updated: env.svc.now().Unix()}
	env.svc.chatMu.Lock()
	env.svc.chatJobs[old.ID] = old
	env.svc.chatMu.Unlock()
	thread, err = env.svc.ChatThread("alice", "")
	if err != nil {
		t.Fatalf("old thread: %v", err)
	}
	if len(thread.Jobs) != 0 {
		t.Fatalf("expired job was returned: %+v", thread.Jobs)
	}
}

func TestChatThreadIncludesSideChatSessionsOnly(t *testing.T) {
	env := newChatTestEnv(t)
	// Seed the map directly: this test is about the endpoint's projection,
	// not the send state machine (covered elsewhere).
	now := env.svc.now().Unix()
	for _, j := range []*ChatJob{
		{ID: "mc_main", AccountID: env.account.ID, PeerID: "alice", SessionID: "", Session: "main",
			Text: "main text", Status: ChatStatusReplied, CreatedAt: now, Updated: now},
		{ID: "mc_side", AccountID: env.account.ID, PeerID: "alice", SessionID: "s_side", Session: "s_side",
			Text: "side text", Status: ChatStatusWaiting, CreatedAt: now + 1, Updated: now + 1},
	} {
		env.svc.chatMu.Lock()
		env.svc.chatJobs[j.ID] = j
		env.svc.chatMu.Unlock()
	}
	thread, err := env.svc.ChatThread("alice", "s_side")
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if len(thread.Jobs) != 1 || thread.Jobs[0].ChatID != "mc_side" || thread.Jobs[0].Text != "side text" {
		t.Fatalf("side projection: %+v", thread.Jobs)
	}
	if len(thread.Sessions) != 1 || thread.Sessions[0] != "s_side" {
		t.Fatalf("session list: %+v", thread.Sessions)
	}
}

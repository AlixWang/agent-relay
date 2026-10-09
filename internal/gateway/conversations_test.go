package gateway

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/config"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/verify"
)

// Test setup helpers

func setupTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		MaxBodyBytes:        1 << 20,
		ConvMaxMembers:      20,
		ConvAgentTurnBudget: 6,
		ConvFuseMaxMessages: 100,
		PermissionTTLSecs:   600,
		FuseMaxMessages:     100,
		FuseMaxAgeSecs:      3600,
		RatePerMinute:       60,
	}

	au := auth.New(st, 3600)
	grd := guard.New(st, guard.Limits{
		FuseMaxMessages:     cfg.FuseMaxMessages,
		FuseMaxAgeSecs:      int64(cfg.FuseMaxAgeSecs),
		RatePerMinute:       cfg.RatePerMinute,
		ConvAgentTurnBudget: cfg.ConvAgentTurnBudget,
		ConvFuseMaxMessages: cfg.ConvFuseMaxMessages,
	})
	qSvc := queue.New(st, grd)
	pres := presence.New(st, 300, "")
	ver := verify.New(st, 600)

	srv := New(cfg, st, au, qSvc, pres, ver, "http://127.0.0.1:18789")
	srv.SetGuard(grd)

	return srv, st
}

func createPeer(t *testing.T, st store.Store, id string) string {
	t.Helper()
	if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatalf("create peer %s: %v", id, err)
	}
	return id
}

func getPeerToken(t *testing.T, srv *Server, peerID string) string {
	t.Helper()
	token, err := srv.auth.Rotate(peerID, "test", time.Now().Unix())
	if err != nil {
		t.Fatalf("issue token for %s: %v", peerID, err)
	}
	return token
}

func doRequest(t *testing.T, srv *Server, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}

	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	w := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(w, req)
	return w
}

func mustDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(w.Body).Decode(v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, w.Body.String())
	}
}

// ---- Integration Tests ----

func TestCreateConversationAPI(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	createPeer(t, st, "bob")
	createPeer(t, st, "charlie")

	token := getPeerToken(t, srv, alice)

	// Create conversation
	req := map[string]any{
		"type":       "group",
		"title":      "Test Group",
		"member_ids": []string{"bob", "charlie"},
	}
	w := doRequest(t, srv, "POST", "/conversations", req, token)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	mustDecode(t, w, &resp)
	if !resp["ok"].(bool) {
		t.Fatal("expected ok=true")
	}
	convID := resp["conv_id"].(string)
	if convID == "" {
		t.Fatal("expected conv_id in response")
	}

	// Verify conversation in DB
	conv, err := st.GetConversation(convID)
	if err != nil || conv == nil {
		t.Fatalf("conversation not created: %v", err)
	}
	if conv.Title != "Test Group" || conv.CreatedBy != "alice" {
		t.Fatalf("wrong conversation data: %+v", conv)
	}

	// Verify members
	members, _ := st.ListConversationMembers(convID)
	if len(members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(members))
	}
}

func TestListConversationsAPI(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation with alice
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Alice's Group",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	token := getPeerToken(t, srv, alice)

	// List conversations
	w := doRequest(t, srv, "GET", "/conversations", nil, token)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	mustDecode(t, w, &resp)
	convs := resp["conversations"].([]any)
	if len(convs) != 1 {
		t.Fatalf("expected 1 conversation, got %d", len(convs))
	}

	conv0 := convs[0].(map[string]any)
	if conv0["id"] != "conv-1" || conv0["title"] != "Alice's Group" {
		t.Fatalf("wrong conversation: %+v", conv0)
	}
}

func TestGetConversationMessagesAPI(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	// Insert messages
	st.InsertMessage(&store.Message{
		ID: "m1", Sender: alice, Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "hello", CreatedAt: 1001,
		ConvID: "conv-1", ApprovalState: "n/a",
	})
	st.InsertMessage(&store.Message{
		ID: "m2", Sender: bob, Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "hi", CreatedAt: 1002,
		ConvID: "conv-1", ApprovalState: "n/a",
	})

	token := getPeerToken(t, srv, alice)

	// Get messages
	w := doRequest(t, srv, "GET", "/conversations/conv-1/messages", nil, token)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	mustDecode(t, w, &resp)
	messages := resp["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(messages))
	}
}

func TestLeaveConversationAPI(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	token := getPeerToken(t, srv, bob)

	// Leave conversation
	w := doRequest(t, srv, "POST", "/conversations/conv-1/leave", nil, token)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify left_at is set
	member, err := st.GetConversationMember("conv-1", bob)
	if err != nil || member.LeftAt == 0 {
		t.Fatalf("left_at not set: %+v, err=%v", member, err)
	}

	// Verify count decreased
	count, _ := st.CountConversationMembers("conv-1")
	if count != 1 {
		t.Fatalf("expected 1 active member, got %d", count)
	}
}

func TestSendMessageToConversation(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	token := getPeerToken(t, srv, alice)

	// Send message
	req := map[string]any{
		"id":       "msg-1",
		"to":       "conv:conv-1",
		"from":     alice,
		"kind":     "chat",
		"payload":  "hello @bob",
		"conv_id":  "conv-1",
		"mentions": []string{"bob"},
	}
	w := doRequest(t, srv, "POST", "/messages", req, token)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify message in DB
	messages, _ := st.VisibleTo(bob, 0, 50)
	if len(messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(messages))
	}
	if messages[0].ConvID != "conv-1" || messages[0].Mentions != `["bob"]` {
		t.Fatalf("wrong message: %+v", messages[0])
	}
}

func TestConversationMemberValidationRejects(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")
	charlie := createPeer(t, st, "charlie")

	// Create conversation with alice and bob only
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	token := getPeerToken(t, srv, charlie)

	// Charlie tries to send - should be rejected
	req := map[string]any{
		"id":      "msg-1",
		"to":      "conv:conv-1",
		"from":    charlie,
		"kind":    "chat",
		"payload": "hello",
		"conv_id": "conv-1",
	}
	w := doRequest(t, srv, "POST", "/messages", req, token)
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["ok"].(bool) {
		t.Fatal("expected ok=false")
	}
}

func TestConversationFreshnessCheckRejects(t *testing.T) {
	srv, st := setupTestServer(t)
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: alice, CreatedAt: 1000,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", alice, "creator", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	aliceToken := getPeerToken(t, srv, alice)
	bobToken := getPeerToken(t, srv, bob)

	// Alice sends first message
	req := map[string]any{
		"id":      "msg-1",
		"to":      "conv:conv-1",
		"from":    alice,
		"kind":    "chat",
		"payload": "hello",
		"conv_id": "conv-1",
	}
	doRequest(t, srv, "POST", "/messages", req, aliceToken)

	// Bob replies without seen_seq - should be rejected
	req = map[string]any{
		"id":       "msg-2",
		"to":       "conv:conv-1",
		"from":     bob,
		"kind":     "chat",
		"payload":  "hi",
		"conv_id":  "conv-1",
		"seen_seq": 0,
	}
	w := doRequest(t, srv, "POST", "/messages", req, bobToken)
	if w.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	mustDecode(t, w, &resp)
	if resp["latest_seq"] == nil {
		t.Fatal("expected latest_seq in response")
	}
}

func TestConversationTurnBudgetRejects(t *testing.T) {
	srv, st := setupTestServer(t)
	createPeer(t, st, "user")
	alice := createPeer(t, st, "alice")
	bob := createPeer(t, st, "bob")

	// Create conversation
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test",
		CreatedBy: "user", CreatedAt: 1000, AgentStreak: 0,
	}
	st.CreateConversation(conv)
	st.AddConversationMember("conv-1", "user", "creator", 0)
	st.AddConversationMember("conv-1", alice, "member", 0)
	st.AddConversationMember("conv-1", bob, "member", 0)

	aliceToken := getPeerToken(t, srv, alice)
	bobToken := getPeerToken(t, srv, bob)

	// Send 6 agent messages (budget = 6)
	for i := 1; i <= 6; i++ {
		token := aliceToken
		sender := alice
		if i%2 == 0 {
			token = bobToken
			sender = bob
		}
		req := map[string]any{
			"id":       "msg-" + string(rune('0'+i)),
			"to":       "conv:conv-1",
			"from":     sender,
			"kind":     "chat",
			"payload":  "message",
			"conv_id":  "conv-1",
			"seen_seq": i - 1,
		}
		w := doRequest(t, srv, "POST", "/messages", req, token)
		if w.Code != 200 {
			t.Fatalf("message %d failed: %d %s", i, w.Code, w.Body.String())
		}
	}

	// 7th agent message should be rejected
	req := map[string]any{
		"id":       "msg-7",
		"to":       "conv:conv-1",
		"from":     alice,
		"kind":     "chat",
		"payload":  "too many",
		"conv_id":  "conv-1",
		"seen_seq": 6,
	}
	w := doRequest(t, srv, "POST", "/messages", req, aliceToken)
	if w.Code != 409 {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

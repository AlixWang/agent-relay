package muse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

// mainChatUpstream models the behavior production exposed: an omitted
// session_id on POST /chat/stream writes the member main chat, while an
// omitted session_id on GET /chat/history reads no session and returns an
// empty history. The main chat is addressable only by the id from
// GET /api/session/list.
type mainChatUpstream struct {
	mainID string

	mu                sync.Mutex
	sessionListCalls  int
	mainExplicitSends int
	implicitSends     int
	sideSends         int
	lastSendSessionID string
	nextSeq           int64
	events            map[string][]map[string]any
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
		switch sid {
		case m.mainID:
			m.mainExplicitSends++
		case "":
			// Production behavior: no id means write the main chat.
			sid = m.mainID
			m.implicitSends++
		default:
			m.sideSends++
		}
		seq := m.addEventLocked(sid, "message.user", text)
		m.mu.Unlock()
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

func newLiveChatSessionForTest(t *testing.T, wsURL string) *liveChatSession {
	t.Helper()
	cc := dialChatTestConn(t, wsURL)
	return &liveChatSession{cc: cc, accountID: "acct-test"}
}

func TestLiveChatSessionResolvesMainChatForSendAndHistory(t *testing.T) {
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
	if !res.Wrote || !res.Echo {
		t.Fatalf("send result: %+v", res)
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

	sessionLists, mainExplicit, implicit, side, lastSID := m.counts()
	if sessionLists != 1 {
		t.Fatalf("session list calls=%d, want one cached lookup", sessionLists)
	}
	if mainExplicit != 1 || implicit != 0 || side != 0 {
		t.Fatalf("send address: explicit-main=%d implicit=%d side=%d", mainExplicit, implicit, side)
	}
	if lastSID != m.mainID {
		t.Fatalf("stream session_id=%q, want %q", lastSID, m.mainID)
	}
}

func TestLiveChatSessionSideChatIsNotRemapped(t *testing.T) {
	m := newMainChatUpstream()
	ws := musetest.New(t)
	ws.SetStreamHandler(m.handle)
	sess := newLiveChatSessionForTest(t, ws.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := sess.Send(ctx, "side-1", "side text", "node-1"); err != nil {
		t.Fatalf("send: %v", err)
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

	_, err := sess.Send(context.Background(), "", "must not send", "node-1")
	if err == nil {
		t.Fatal("send succeeded without a resolvable main chat")
	}
	cf := chatFailureAs(err)
	if cf.Code != "muse_main_chat_unresolved" || cf.Wrote {
		t.Fatalf("failure: %+v", cf)
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
	if got.BaselineSeq != 0 || got.UserSeq == 0 || got.ConfirmedAt == 0 {
		t.Fatalf("snapshot: %+v", got)
	}
	if got.Error != nil {
		t.Fatalf("unexpected error: %+v", got.Error)
	}
	rows, err := env.st.ListAudit("", "muse.chat.sent", 0, 20)
	if err != nil || len(rows) != 1 {
		t.Fatalf("sent audit rows=%d err=%v", len(rows), err)
	}
	if !strings.Contains(rows[0].Detail, "delivery=confirmed") {
		t.Fatalf("sent audit detail: %q", rows[0].Detail)
	}

	if _, err := env.svc.CancelChat(context.Background(), view.ChatID, "admin"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	waitChatDone(t, env.svc, view.ChatID)
	if _, mainExplicit, implicit, _, _ := m.counts(); mainExplicit != 1 || implicit != 0 {
		t.Fatalf("orchestrator address: explicit-main=%d implicit=%d", mainExplicit, implicit)
	}
}

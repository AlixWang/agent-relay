package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/stream"
)

// The console speaks as the reserved operator identity and — the regression
// this whole design exists for — its message must wake SSE subscribers. The
// v0.14.0 conversations path wrote rows straight into the store, so an
// operator message was invisible to every SSE assistant until reconnect.
func TestAdminSendWakesSSEAndSpeaksAsOperator(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	cookie := f.adminLogin(t)
	f.srv.SetStream(stream.New(3, 100))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/messages/stream?for=alice&since=0", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.mux.ServeHTTP(rec, req); close(done) }()
	time.Sleep(200 * time.Millisecond) // let it subscribe

	code, out := f.doCookie(t, "POST", "/admin/messages", map[string]any{
		"to": "alice", "kind": "task", "payload": "控制台指令：把当前进度汇报一下",
	}, cookie)
	if code != 200 || out["ok"] != true {
		t.Fatalf("admin send: %d %+v", code, out)
	}
	if out["thread"] != "op/alice" {
		t.Fatalf("operator DM thread must be canonical: %v", out["thread"])
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(rec.Body.String(), "控制台指令") {
		select {
		case <-done:
			t.Fatalf("stream closed before the operator frame arrived: %q", rec.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("operator message did not reach the held stream: %q", rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	// The receiver sees the reserved sender, and can reply to it.
	_, pull := f.do(t, "GET", "/messages?for=alice&since=0", nil, f.tok["alice"])
	items, _ := pull["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("pull: %+v", pull)
	}
	first, _ := items[0].(map[string]any)
	if first["from"] != "operator" || first["kind"] != "task" {
		t.Fatalf("operator message shape: %+v", first)
	}
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "reply-1", "to": "operator", "kind": "result",
		"in_reply_to": first["id"], "payload": "进度：三条已完成，一条在跑",
	}, f.tok["alice"]); code != 200 || out["ok"] != true {
		t.Fatalf("agent reply to operator: %d %+v", code, out)
	}
}

// The operator identity is not settable from the body: readJSON rejects unknown
// fields, so a "from" key is an error rather than a silent impersonation.
func TestAdminSendRejectsFromOverride(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	cookie := f.adminLogin(t)
	if code, _ := f.doCookie(t, "POST", "/admin/messages", map[string]any{
		"to": "alice", "payload": "x", "from": "alice",
	}, cookie); code != 400 {
		t.Fatalf("from override must be rejected, got %d", code)
	}
	if code, _ := f.doCookie(t, "POST", "/admin/messages", map[string]any{
		"to": "alice", "payload": "   ",
	}, cookie); code != 400 {
		t.Fatalf("empty payload must be rejected, got %d", code)
	}
}

func TestRoomLifecycleMembershipAndNotices(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	f.registerPeer(t, "carol", "muse")
	cookie := f.adminLogin(t)

	code, out := f.doCookie(t, "POST", "/admin/rooms", map[string]any{
		"id": "ops", "name": "运维群", "members": []string{"alice", "bob"},
	}, cookie)
	if code != 200 || out["ok"] != true {
		t.Fatalf("create room: %d %+v", code, out)
	}
	room, _ := out["room"].(map[string]any)
	if room["id"] != "grp_ops" || room["name"] != "运维群" {
		t.Fatalf("room shape: %+v", room)
	}
	members, _ := room["members"].([]any)
	if len(members) != 2 {
		t.Fatalf("members: %+v", members)
	}

	// Duplicate and malformed ids are refused.
	if code, _ := f.doCookie(t, "POST", "/admin/rooms", map[string]any{"id": "ops", "members": []string{"alice"}}, cookie); code != 409 {
		t.Fatalf("duplicate room should be 409, got %d", code)
	}
	if code, _ := f.doCookie(t, "POST", "/admin/rooms", map[string]any{"id": "Bad Slug!", "members": []string{"alice"}}, cookie); code != 400 {
		t.Fatalf("bad room id should be 400, got %d", code)
	}
	if code, _ := f.doCookie(t, "POST", "/admin/rooms", map[string]any{"id": "empty"}, cookie); code != 200 {
		t.Fatalf("room without members should be allowed (add later), got %d", code)
	}
	if code, _ := f.doCookie(t, "POST", "/admin/rooms/grp_ops/members", map[string]any{"peer_id": "nobody"}, cookie); code != 400 {
		t.Fatalf("unknown peer should be 400, got %d", code)
	}

	// Adding a member posts a notice into the room (roster changes are part of
	// the conversation, and the new member must see it).
	if code, _ := f.doCookie(t, "POST", "/admin/rooms/grp_ops/members", map[string]any{"peer_id": "carol"}, cookie); code != 200 {
		t.Fatalf("add member failed: %d", code)
	}
	_, list := f.doCookie(t, "GET", "/admin/rooms", nil, cookie)
	rooms, _ := list["rooms"].([]any)
	if len(rooms) != 2 {
		t.Fatalf("rooms: %+v", list)
	}
	_, thread := f.doCookie(t, "GET", "/admin/messages?thread=grp_ops", nil, cookie)
	items, _ := thread["items"].([]any)
	notices := 0
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row["sender"] == "system" && row["kind"] == "system" {
			notices++
		}
	}
	if notices != 2 { // 创建通知 + carol 加入通知
		t.Fatalf("expected 2 roster notices in the room thread, got %d: %+v", notices, thread)
	}
	roomInfo, _ := thread["room"].(map[string]any)
	if roomInfo == nil {
		t.Fatalf("room info missing from thread view: %+v", thread)
	}
	if got, _ := roomInfo["members"].([]any); len(got) != 3 {
		t.Fatalf("room members in thread view: %+v", roomInfo)
	}

	// A member agent can post into the room; a non-member cannot (403).
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "room-1", "to": "grp_ops", "kind": "chat", "payload": "我在，先把部署窗口对一下",
	}, f.tok["alice"]); code != 200 || out["ok"] != true {
		t.Fatalf("member room send: %d %+v", code, out)
	}
	// carol was added above, so she is a member and may speak.
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "room-2", "to": "grp_ops", "kind": "chat", "payload": "carol 刚被拉进来，这条应该成功",
	}, f.tok["carol"]); code != 200 || out["ok"] != true {
		t.Fatalf("added member room send: %d %+v", code, out)
	}
	f.registerPeer(t, "dave", "muse")
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "room-3", "to": "grp_ops", "kind": "chat", "payload": "局外人想插一句但没人拉他进来",
	}, f.tok["dave"]); code != 403 {
		t.Fatalf("outsider must be refused with 403, got %d %+v", code, out)
	}
	if code, _ := f.do(t, "POST", "/messages", map[string]any{
		"id": "room-4", "to": "grp_nope", "kind": "chat", "payload": "typo target",
	}, f.tok["alice"]); code != 400 {
		t.Fatalf("unknown room must be 400, got %d", code)
	}

	// The sender does not get its own room message back (no self-wake loop),
	// the other members do.
	_, aliceView := f.do(t, "GET", "/messages?for=alice&since=0", nil, f.tok["alice"])
	if strings.Contains(bodyString(aliceView), "先把部署窗口对一下") {
		t.Fatalf("sender saw its own room message: %+v", aliceView)
	}
	_, bobView := f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if !strings.Contains(bodyString(bobView), "先把部署窗口对一下") {
		t.Fatalf("other member did not receive the room message: %+v", bobView)
	}

	// Removing a member stops delivery and leaves a notice.
	if code, _ := f.doCookie(t, "DELETE", "/admin/rooms/grp_ops/members/bob", nil, cookie); code != 200 {
		t.Fatalf("remove member failed: %d", code)
	}
	if _, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "room-5", "to": "grp_ops", "kind": "chat", "payload": "bob 已经被移出了这一条它不该看到",
	}, f.tok["alice"]); out["ok"] != true {
		t.Fatalf("member send after removal: %+v", out)
	}
	_, bobView = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if strings.Contains(bodyString(bobView), "不该看到") {
		t.Fatalf("removed member still receives room messages: %+v", bobView)
	}

	// Archiving: refused for new sends, hidden from the default console list.
	if code, _ := f.doCookie(t, "PATCH", "/admin/rooms/grp_ops", map[string]any{"archived": true}, cookie); code != 200 {
		t.Fatalf("archive failed: %d", code)
	}
	if code, _ := f.doCookie(t, "POST", "/admin/messages", map[string]any{"to": "grp_ops", "payload": "x"}, cookie); code != 400 {
		t.Fatalf("archived room send must be refused, got %d", code)
	}
	_, list = f.doCookie(t, "GET", "/admin/rooms", nil, cookie)
	if got, _ := list["rooms"].([]any); len(got) != 1 {
		t.Fatalf("archived room should be hidden: %+v", got)
	}
	_, list = f.doCookie(t, "GET", "/admin/rooms?archived=1", nil, cookie)
	if got, _ := list["rooms"].([]any); len(got) != 2 {
		t.Fatalf("archived room should appear with archived=1: %+v", got)
	}
}

func TestOperatorInboxAndReadWatermark(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	cookie := f.adminLogin(t)

	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "r1", "to": "operator", "kind": "result", "payload": "部署完成，服务已重启",
	}, f.tok["alice"]); code != 200 || out["ok"] != true {
		t.Fatalf("agent → operator: %d %+v", code, out)
	}

	_, inbox := f.doCookie(t, "GET", "/admin/inbox", nil, cookie)
	items, _ := inbox["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("inbox: %+v", inbox)
	}
	first, _ := items[0].(map[string]any)
	if first["from"] != "alice" || first["unread"] != true {
		t.Fatalf("inbox item: %+v", first)
	}
	if unread, _ := inbox["unread"].(float64); unread != 1 {
		t.Fatalf("unread count: %+v", inbox["unread"])
	}
	_, stats := f.doCookie(t, "GET", "/admin/stats", nil, cookie)
	if got, _ := stats["inbox_unread"].(float64); got != 1 {
		t.Fatalf("stats inbox_unread: %+v", stats["inbox_unread"])
	}

	seq, _ := first["seq"].(float64)
	if code, _ := f.doCookie(t, "POST", "/admin/inbox/read", map[string]any{"seq": int64(seq)}, cookie); code != 200 {
		t.Fatalf("mark read failed: %d", code)
	}
	_, inbox = f.doCookie(t, "GET", "/admin/inbox", nil, cookie)
	if unread, _ := inbox["unread"].(float64); unread != 0 {
		t.Fatalf("unread after read: %+v", inbox["unread"])
	}
	// The watermark only moves forward: a stale tab cannot unread messages.
	if _, out := f.doCookie(t, "POST", "/admin/inbox/read", map[string]any{"seq": 0}, cookie); out["read_seq"].(float64) != seq {
		t.Fatalf("read marker moved backwards: %+v", out)
	}
}

// Room threads expose per-member ack state so the console can show who picked a
// message up, and a room reply to the operator lands in the DM thread.
func TestRoomThreadAcksAndOperatorDMThread(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	cookie := f.adminLogin(t)
	if code, out := f.doCookie(t, "POST", "/admin/rooms", map[string]any{
		"id": "ops", "members": []string{"alice", "bob"},
	}, cookie); code != 200 {
		t.Fatalf("room: %d %+v", code, out)
	}
	if code, out := f.doCookie(t, "POST", "/admin/messages", map[string]any{
		"to": "grp_ops", "kind": "task", "payload": "@alice 把证书到期时间查一下",
	}, cookie); code != 200 {
		t.Fatalf("room task: %d %+v", code, out)
	}
	// A member also receives the room's roster notices; pick the task out.
	_, pull := f.do(t, "GET", "/messages?for=alice&since=0", nil, f.tok["alice"])
	items, _ := pull["items"].([]any)
	var msg map[string]any
	for _, it := range items {
		row, _ := it.(map[string]any)
		if row["kind"] == "task" {
			msg = row
		}
	}
	if msg == nil {
		t.Fatalf("alice did not receive the room task: %+v", pull)
	}
	if code, _ := f.do(t, "POST", "/ack", map[string]any{
		"message_id": msg["id"], "by": "alice",
	}, f.tok["alice"]); code != 200 {
		t.Fatalf("ack failed: %d", code)
	}
	_, thread := f.doCookie(t, "GET", "/admin/messages?thread=grp_ops", nil, cookie)
	threadItems, _ := thread["items"].([]any)
	acked := false
	for _, it := range threadItems {
		row, _ := it.(map[string]any)
		if row["id"] != msg["id"] {
			continue
		}
		who, _ := row["acked_by"].([]any)
		if len(who) == 1 && who[0] == "alice" {
			acked = true
		}
	}
	if !acked {
		t.Fatalf("room thread missing per-member acks: %+v", thread)
	}

	// Alice replies to the operator: one DM thread per peer, not per message.
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "dm-1", "to": "operator", "kind": "result", "payload": "证书 2027-03 到期",
	}, f.tok["alice"]); code != 200 {
		t.Fatalf("dm: %d %+v", code, out)
	}
	if code, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "dm-2", "to": "operator", "kind": "chat", "payload": "另外顺手把续期脚本也跑了",
	}, f.tok["alice"]); code != 200 {
		t.Fatalf("dm2: %d %+v", code, out)
	}
	_, thread = f.doCookie(t, "GET", "/admin/messages?thread=op/alice", nil, cookie)
	if got, _ := thread["items"].([]any); len(got) != 2 {
		t.Fatalf("operator DM thread should hold both messages: %+v", thread)
	}
}

func TestConsoleEndpointsRequireAdminSession(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	routes := [][3]any{
		{"GET", "/admin/rooms", nil},
		{"POST", "/admin/rooms", map[string]any{"id": "x", "members": []string{"alice"}}},
		{"PATCH", "/admin/rooms/grp_x", map[string]any{"name": "n"}},
		{"POST", "/admin/rooms/grp_x/members", map[string]any{"peer_id": "alice"}},
		{"DELETE", "/admin/rooms/grp_x/members/alice", nil},
		{"POST", "/admin/messages", map[string]any{"to": "alice", "payload": "x"}},
		{"GET", "/admin/inbox", nil},
		{"POST", "/admin/inbox/read", map[string]any{"seq": 1}},
	}
	for _, r := range routes {
		method, path := r[0].(string), r[1].(string)
		if code, _ := f.do(t, method, path, r[2], f.tok["alice"]); code != 401 {
			t.Fatalf("%s %s with a peer token must be 401, got %d", method, path, code)
		}
		if code, _ := f.do(t, method, path, r[2], ""); code != 401 {
			t.Fatalf("%s %s without a session must be 401, got %d", method, path, code)
		}
	}
}

// bodyString renders a JSON-decoded map for substring assertions.
func bodyString(v map[string]any) string {
	var b strings.Builder
	for _, item := range v["items"].([]any) {
		row, _ := item.(map[string]any)
		if s, ok := row["payload"].(string); ok {
			b.WriteString(s)
			b.WriteString("\n")
		}
	}
	return b.String()
}

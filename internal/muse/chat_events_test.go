package muse

import (
	"encoding/json"
	"testing"
)

func TestParseChatHistoryResultEnvelope(t *testing.T) {
	raw := []byte(`{"result":{"chat_events":[{"event_name":"message.user","seq":7,"message_id":"m7","payload":{"display_text":"pwd"}}],"has_more":true}}`)
	h, err := parseChatHistory(raw)
	if err != nil {
		t.Fatalf("parse wrapped history: %v", err)
	}
	if len(h.ChatEvents) != 1 || h.ChatEvents[0].Seq != 7 || h.ChatEvents[0].Payload.DisplayText != "pwd" {
		t.Fatalf("wrapped history: %+v", h)
	}
	if !h.HasMore {
		t.Fatal("wrapped has_more was lost")
	}
}

func TestParseChatHistoryEnvelopeShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		ok   bool
	}{
		{name: "bare", raw: `{"chat_events":[],"has_more":false}`, ok: true},
		{name: "wrapped", raw: `{"result":{"chat_events":[],"has_more":true}}`, ok: true},
		{name: "malformed", raw: `{"chat_events":`, ok: false},
		{name: "missing", raw: `{}`, ok: false},
		{name: "nested result", raw: `{"result":{"result":{"chat_events":[]}}}`, ok: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := parseChatHistory([]byte(tc.raw))
			if tc.ok {
				if err != nil || h == nil || h.ChatEvents == nil {
					t.Fatalf("parse history: h=%+v err=%v", h, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected protocol error, got %+v", h)
			}
		})
	}
}

func histEvent(event string, seq int64, session, text, status string) chatEvent {
	return chatEvent{
		EventName: event, Seq: seq, MessageID: event + "-" + strconvI(seq),
		Payload: chatPayload{SessionID: session, DisplayText: text, Content: text, Status: status},
	}
}

func strconvI(n int64) string { return fmtInt(n) }

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func TestParseChatEventsReplyCriteria(t *testing.T) {
	base := int64(100)
	// P1: normal completed reply.
	res := parseChatEvents([]chatEvent{
		histEvent("message.user", 101, "s", "hello", ""),
		histEvent("message.assistant", 102, "s", "hi there", "completed"),
	}, base, "s", 101)
	if res.Reply == nil || res.Reply.Seq != 102 || res.Reply.Text != "hi there" || res.Reply.MediaOnly || res.Ambiguous {
		t.Fatalf("P1: %+v", res)
	}

	// P2: either reply_to field non-empty is a push, not a reply.
	pushTop := histEvent("message.assistant", 103, "s", "push", "completed")
	pushTop.ReplyToMessageID = "x"
	pushPayload := histEvent("message.assistant", 104, "s", "push2", "completed")
	pushPayload.Payload.ReplyToMessageID = "y"
	res = parseChatEvents([]chatEvent{pushTop, pushPayload}, base, "s", 0)
	if res.Reply != nil {
		t.Fatalf("P2 picked a push: %+v", res.Reply)
	}

	// P3: draft status empty is retained only as partial text.
	res = parseChatEvents([]chatEvent{histEvent("message.assistant", 105, "s", "typing...", "")}, base, "s", 0)
	if res.Reply != nil || res.DraftText != "typing..." || res.DraftSeq != 105 {
		t.Fatalf("P3: %+v", res)
	}

	// P4/P5: old seq and non-assistant event are ignored.
	res = parseChatEvents([]chatEvent{
		histEvent("message.assistant", 100, "s", "old", "completed"),
		histEvent("message.user", 106, "s", "user", ""),
	}, base, "s", 0)
	if res.Reply != nil {
		t.Fatalf("P4/P5: %+v", res.Reply)
	}

	// P6: empty text/attachments ignored; P6b: media-only reply accepted.
	res = parseChatEvents([]chatEvent{histEvent("message.assistant", 107, "s", "", "completed")}, base, "s", 0)
	if res.Reply != nil {
		t.Fatalf("P6: %+v", res.Reply)
	}
	media := histEvent("message.assistant", 108, "s", "", "completed")
	media.Payload.Data = json.RawMessage(`{"images":[{"path":"sandbox://a.png","mime":"image/png","byte_len":12,"variants":{"original":"https://vm/a.png"}}]}`)
	res = parseChatEvents([]chatEvent{media}, base, "s", 0)
	if res.Reply == nil || !res.Reply.MediaOnly || res.Reply.Text != "" ||
		len(res.Reply.Attachments) != 1 || res.Reply.Attachments[0].Path != "sandbox://a.png" ||
		res.Reply.Attachments[0].Variants["original"] != "https://vm/a.png" {
		t.Fatalf("P6b/P9: %+v", res.Reply)
	}

	// P7: scope mismatch (thread main / wrong side sid).
	thread := histEvent("message.assistant", 109, "", "thread", "completed")
	thread.Payload.IsThread = true
	res = parseChatEvents([]chatEvent{thread}, base, "", 0)
	if res.Reply != nil {
		t.Fatalf("P7 main thread: %+v", res.Reply)
	}
	res = parseChatEvents([]chatEvent{histEvent("message.assistant", 110, "other", "nope", "completed")}, base, "wanted", 0)
	if res.Reply != nil {
		t.Fatalf("P7 side: %+v", res.Reply)
	}

	// P8: first seq wins, rest become additional_replies.
	res = parseChatEvents([]chatEvent{
		histEvent("message.assistant", 112, "s", "second", "completed"),
		histEvent("message.assistant", 111, "s", "first", "completed"),
	}, base, "s", 0)
	if res.Reply == nil || res.Reply.Seq != 111 || len(res.Additional) != 1 || res.Additional[0].Seq != 112 {
		t.Fatalf("P8: %+v", res)
	}

	// P10: delta.presentation is collected, never a reply.
	pres := histEvent("delta.presentation", 113, "s", "image", "")
	pres.Payload.Kind = "image"
	pres.Payload.Data = json.RawMessage(`{"file":{"path":"sandbox://p.png"}}`)
	res = parseChatEvents([]chatEvent{pres}, base, "s", 0)
	if res.Reply != nil || len(res.Presentations) != 1 || res.Presentations[0].Kind != "image" {
		t.Fatalf("P10: %+v", res)
	}

	// P11: another user between our user_seq and the reply -> ambiguous.
	res = parseChatEvents([]chatEvent{
		histEvent("message.user", 114, "s", "mine", ""),
		histEvent("message.user", 115, "s", "member", ""),
		histEvent("message.assistant", 116, "s", "answer", "completed"),
	}, base, "s", 114)
	if res.Reply == nil || !res.Ambiguous {
		t.Fatalf("P11: %+v", res)
	}
	if !hasString(res.Warnings, "concurrent_user_activity") {
		t.Fatalf("P11 warnings: %+v", res.Warnings)
	}

	// P12: duplicate seq/message_id is returned once and draft ignored.
	dup := histEvent("message.assistant", 117, "s", "once", "completed")
	res = parseChatEvents([]chatEvent{dup, dup}, 100, "s", 0)
	if res.Reply == nil || res.Reply.Seq != 117 {
		t.Fatalf("P12: %+v", res)
	}

	// reply_status_not_completed warning and isolate stripping.
	iso := histEvent("message.assistant", 118, "s", "\u2068hello\u2069", "failed")
	res = parseChatEvents([]chatEvent{iso}, 100, "s", 0)
	if res.Reply == nil || res.Reply.Text != "hello" || !hasString(res.Warnings, "reply_status_not_completed") {
		t.Fatalf("status/isolate: %+v", res)
	}
	approval := histEvent("message.assistant", 119, "s", "please approve this request", "completed")
	res = parseChatEvents([]chatEvent{approval}, 100, "s", 0)
	if !hasString(res.Warnings, "may_need_native_approval") {
		t.Fatalf("approval warning: %+v", res.Warnings)
	}
}

func TestFindChatUserNormalizesAndDetectsDuplicate(t *testing.T) {
	sent := "  hello\u2068 world \u2069 "
	events := []chatEvent{
		{EventName: "message.user", Seq: 10, Payload: chatPayload{DisplayText: "hello\u2068 world\u2069"}},
		{EventName: "message.user", Seq: 11, Payload: chatPayload{Content: "  hello world  "}},
	}
	seq, dup, found := findChatUser(events, 9, sent)
	if !found || seq != 10 || !dup {
		t.Fatalf("find user: seq=%d dup=%v found=%v", seq, dup, found)
	}
	// baseline excludes older matching text.
	seq, dup, found = findChatUser(events, 10, sent)
	if !found || seq != 11 || dup {
		t.Fatalf("baseline filter: seq=%d dup=%v found=%v", seq, dup, found)
	}
	if chatTextHash(" hello ") != chatTextHash("hello") {
		t.Fatal("hash must normalize surrounding whitespace")
	}
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

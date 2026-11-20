package muse

// chat_events.go is the pure reply parser for chat.history. It deliberately
// does no I/O: every judgement about what counts as a real assistant reply,
// media metadata, scope, drafts and concurrent-user confidence is table-driven
// and testable without a Noise server.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	chatIsolateL = '\u2068'
	chatIsolateR = '\u2069'
)

type chatHistoryResponse struct {
	ChatEvents []chatEvent          `json:"chat_events"`
	HasMore    bool                 `json:"has_more"`
	Result     *chatHistoryResponse `json:"result"`
}

type chatEvent struct {
	EventName        string      `json:"event_name"`
	Seq              int64       `json:"seq"`
	MessageID        string      `json:"message_id"`
	Role             string      `json:"role"`
	OccurredAtMS     int64       `json:"occurred_at_ms"`
	DisplayTextAtMS  int64       `json:"display_text_updated_at_ms"`
	ReplyToMessageID string      `json:"reply_to_message_id"`
	Payload          chatPayload `json:"payload"`
}

type chatPayload struct {
	Role             string          `json:"role"`
	MessageID        string          `json:"message_id"`
	SessionID        string          `json:"session_id"`
	IsThread         bool            `json:"is_thread"`
	DisplayText      string          `json:"display_text"`
	Content          string          `json:"content"`
	Status           string          `json:"status"`
	DisplayTextReady bool            `json:"display_text_ready"`
	ReplyToMessageID string          `json:"reply_to_message_id"`
	Kind             string          `json:"kind"`
	Data             json.RawMessage `json:"data"`
	Widgets          json.RawMessage `json:"widgets"`
}

// ChatAttachment is metadata only; v1 never downloads media (D14).
type ChatAttachment struct {
	Kind     string            `json:"kind,omitempty"`
	Path     string            `json:"path,omitempty"`
	Mime     string            `json:"mime,omitempty"`
	ByteLen  int64             `json:"byte_len,omitempty"`
	Variants map[string]string `json:"variants,omitempty"`
	Title    string            `json:"title,omitempty"`
	URL      string            `json:"url,omitempty"`
}

type ChatPresentation struct {
	Seq         int64            `json:"seq"`
	Kind        string           `json:"kind,omitempty"`
	Text        string           `json:"text,omitempty"`
	Attachments []ChatAttachment `json:"attachments,omitempty"`
}

type ChatReply struct {
	Seq               int64              `json:"seq"`
	MessageID         string             `json:"message_id"`
	Text              string             `json:"text"`
	Status            string             `json:"status"`
	MediaOnly         bool               `json:"media_only"`
	AtMS              int64              `json:"at_ms"`
	Attachments       []ChatAttachment   `json:"attachments,omitempty"`
	Presentations     []ChatPresentation `json:"presentations,omitempty"`
	AdditionalReplies []ChatReply        `json:"additional_replies,omitempty"`
}

type chatParseResult struct {
	Reply         *ChatReply
	Additional    []ChatReply
	Presentations []ChatPresentation
	DraftText     string
	DraftSeq      int64
	MaxSeq        int64
	Ambiguous     bool
	Warnings      []string
}

func parseChatHistory(raw []byte) (*chatHistoryResponse, error) {
	var out chatHistoryResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out.Result != nil {
		return out.Result, nil
	}
	return &out, nil
}

func normalizeChatText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == chatIsolateL || r == chatIsolateR {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

func chatTextHash(s string) string {
	sum := sha256.Sum256([]byte(normalizeChatText(s)))
	return hex.EncodeToString(sum[:])
}

func eventText(ev chatEvent) string {
	if ev.Payload.DisplayText != "" {
		return normalizeChatText(ev.Payload.DisplayText)
	}
	return normalizeChatText(ev.Payload.Content)
}

func eventMessageID(ev chatEvent) string {
	if ev.Payload.MessageID != "" {
		return ev.Payload.MessageID
	}
	return ev.MessageID
}

func dedupeAndSortEvents(events []chatEvent) []chatEvent {
	out := make([]chatEvent, 0, len(events))
	seen := make(map[string]bool, len(events))
	for _, ev := range events {
		key := fmt.Sprintf("%s\x00%s\x00%d", eventMessageID(ev), ev.EventName, ev.Seq)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ev)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// parseChatEvents applies the v1 reply test exactly: message.assistant, seq
// above baseline, both reply_to_message_id fields empty, some text or agent
// attachment/card, non-empty status, and matching scope. The first candidate by
// seq wins; later candidates are additional_replies. Drafts are retained only
// for a timeout partial_text.
func parseChatEvents(events []chatEvent, baseline int64, sessionID string, userSeq int64) chatParseResult {
	events = dedupeAndSortEvents(events)
	res := chatParseResult{}
	var candidates []ChatReply
	otherUserAfterUs := false
	for _, ev := range events {
		if ev.Seq > res.MaxSeq {
			res.MaxSeq = ev.Seq
		}
		if ev.EventName == "message.user" && userSeq > 0 && ev.Seq > userSeq {
			otherUserAfterUs = true
		}
		if ev.EventName != "message.assistant" || ev.Seq <= baseline {
			continue
		}
		attachments := extractChatAttachments(ev.Payload.Data)
		text := ""
		if ev.Payload.DisplayText != "" {
			text = strings.TrimSpace(stripChatIsolates(ev.Payload.DisplayText))
		} else {
			text = strings.TrimSpace(stripChatIsolates(ev.Payload.Content))
		}
		if ev.ReplyToMessageID != "" || ev.Payload.ReplyToMessageID != "" {
			continue
		}
		if text == "" && len(attachments) == 0 {
			continue
		}
		if !chatScopeMatches(ev, sessionID) {
			continue
		}
		if ev.Payload.Status == "" {
			if text != "" && ev.Seq > res.DraftSeq {
				res.DraftText = text
				res.DraftSeq = ev.Seq
			}
			continue
		}
		candidates = append(candidates, ChatReply{
			Seq: ev.Seq, MessageID: eventMessageID(ev), Text: text,
			Status: ev.Payload.Status, MediaOnly: text == "", AtMS: ev.OccurredAtMS,
			Attachments: attachments,
		})
	}
	for _, ev := range events {
		if ev.EventName != "delta.presentation" || ev.Seq <= baseline {
			continue
		}
		p := ChatPresentation{Seq: ev.Seq, Kind: ev.Payload.Kind,
			Text:        strings.TrimSpace(stripChatIsolates(firstNonEmpty(ev.Payload.DisplayText, ev.Payload.Content))),
			Attachments: extractChatAttachments(ev.Payload.Data)}
		if p.Text == "" && len(p.Attachments) == 0 {
			continue
		}
		res.Presentations = append(res.Presentations, p)
	}
	if len(candidates) == 0 {
		return res
	}
	primary := candidates[0]
	if otherUserAfterUs {
		res.Ambiguous = true
		res.Warnings = append(res.Warnings, "concurrent_user_activity")
	}
	if !strings.EqualFold(primary.Status, "completed") {
		res.Warnings = append(res.Warnings, "reply_status_not_completed")
	}
	if mayNeedNativeApproval(primary.Text) {
		res.Warnings = append(res.Warnings, "may_need_native_approval")
	}
	primary.Presentations = res.Presentations
	if len(candidates) > 1 {
		primary.AdditionalReplies = append([]ChatReply(nil), candidates[1:]...)
	}
	res.Reply = &primary
	res.Additional = candidates[1:]
	res.Warnings = uniqueStrings(res.Warnings)
	return res
}

func chatScopeMatches(ev chatEvent, sessionID string) bool {
	if sessionID != "" {
		return ev.Payload.SessionID == "" || ev.Payload.SessionID == sessionID
	}
	return !ev.Payload.IsThread
}

func stripChatIsolates(s string) string {
	return strings.Map(func(r rune) rune {
		if r == chatIsolateL || r == chatIsolateR {
			return -1
		}
		return r
	}, s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func uniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// findChatUser locates our message.user by normalized text hash after baseline.
// It returns the earliest seq and whether the same normalized text appeared
// more than once (which makes reply attribution ambiguous).
func findChatUser(events []chatEvent, baseline int64, sentText string) (seq int64, duplicates bool, found bool) {
	want := chatTextHash(sentText)
	events = dedupeAndSortEvents(events)
	count := 0
	for _, ev := range events {
		if ev.EventName != "message.user" || ev.Seq <= baseline {
			continue
		}
		if chatTextHash(eventText(ev)) != want {
			continue
		}
		count++
		if seq == 0 {
			seq = ev.Seq
		}
	}
	return seq, count > 1, count > 0
}

func mayNeedNativeApproval(text string) bool {
	low := strings.ToLower(text)
	for _, needle := range []string{
		"需要审批", "请求授权", "请求批准", "请批准", "请授权", "允许我",
		"permission", "approve", "approval", "allow me", "authorize",
	} {
		if strings.Contains(low, needle) {
			return true
		}
	}
	return false
}

// extractChatAttachments walks payload.data for agent-produced resources and
// cards. metadata.attachments is intentionally skipped: those are user uploads,
// not the assistant's reply artefact (D14/§1.6).
func extractChatAttachments(raw json.RawMessage) []ChatAttachment {
	if len(raw) == 0 {
		return nil
	}
	var data any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&data); err != nil {
		return nil
	}
	var out []ChatAttachment
	collectChatData(data, "", &out)
	return dedupeAttachments(out)
}

func collectChatData(v any, key string, out *[]ChatAttachment) {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			collectChatData(item, key, out)
		}
	case map[string]any:
		if key == "metadata" {
			return
		}
		switch key {
		case "images", "videos", "files", "file":
			if a, ok := attachmentFromMap(x, singularKind(key)); ok {
				*out = append(*out, a)
			}
		case "external_link", "internal_link_action_card", "space_build_completed", "browser_task":
			if a, ok := cardFromMap(x, key); ok {
				*out = append(*out, a)
			}
		}
		// Known named resource containers, including a single file object.
		for k, child := range x {
			switch k {
			case "images", "videos", "files", "file":
				if arr, ok := child.([]any); ok {
					for _, item := range arr {
						if m, ok := item.(map[string]any); ok {
							if a, ok := attachmentFromMap(m, singularKind(k)); ok {
								*out = append(*out, a)
							}
						}
					}
				} else if m, ok := child.(map[string]any); ok {
					if a, ok := attachmentFromMap(m, singularKind(k)); ok {
						*out = append(*out, a)
					}
				}
			case "external_link", "internal_link_action_card", "space_build_completed", "browser_task":
				if m, ok := child.(map[string]any); ok {
					if a, ok := cardFromMap(m, k); ok {
						*out = append(*out, a)
					}
				}
			}
		}
		// A bare attachment-shaped object nested under a neutral key.
		if key != "images" && key != "videos" && key != "files" && key != "file" &&
			key != "external_link" && key != "internal_link_action_card" &&
			key != "space_build_completed" && key != "browser_task" {
			if _, hasPath := x["path"]; hasPath {
				if a, ok := attachmentFromMap(x, key); ok {
					*out = append(*out, a)
				}
			} else if _, hasVariants := x["variants"]; hasVariants {
				if a, ok := attachmentFromMap(x, key); ok {
					*out = append(*out, a)
				}
			}
		}
	}
}

func singularKind(k string) string {
	switch k {
	case "images":
		return "image"
	case "videos":
		return "video"
	case "files":
		return "file"
	default:
		return k
	}
}

func attachmentFromMap(m map[string]any, kind string) (ChatAttachment, bool) {
	a := ChatAttachment{Kind: kind}
	if p, ok := m["path"].(string); ok {
		a.Path = p
	}
	if a.Path == "" {
		if u := variantsOriginal(m); u != "" {
			a.URL = u
		}
	}
	if a.Path == "" && a.URL == "" {
		return a, false
	}
	a.Mime = strField(m, "mime", "mime_type")
	a.ByteLen = intField(m, "byte_len", "bytes_len", "size")
	a.Variants = variantsMap(m)
	if a.URL != "" {
		if a.Variants == nil {
			a.Variants = map[string]string{"original": a.URL}
		}
	}
	return a, true
}

func cardFromMap(m map[string]any, kind string) (ChatAttachment, bool) {
	a := ChatAttachment{Kind: kind}
	a.Title = strField(m, "title", "display_name", "name")
	a.URL = strField(m, "url")
	if inner, ok := m["data"].(map[string]any); ok {
		if a.URL == "" {
			a.URL = strField(inner, "url")
		}
		if a.Title == "" {
			a.Title = strField(inner, "title", "subtitle")
		}
	}
	if a.Title == "" && a.URL == "" {
		return a, false
	}
	return a, true
}

func variantsMap(m map[string]any) map[string]string {
	raw, ok := m["variants"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]string{}
	for k, v := range raw {
		if s, ok := v.(string); ok && s != "" {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func variantsOriginal(m map[string]any) string {
	if raw, ok := m["variants"].(map[string]any); ok {
		if s, ok := raw["original"].(string); ok {
			return s
		}
	}
	return ""
}

func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func intField(m map[string]any, keys ...string) int64 {
	for _, k := range keys {
		switch v := m[k].(type) {
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n
			}
		case float64:
			return int64(v)
		case int64:
			return v
		}
	}
	return 0
}

func dedupeAttachments(in []ChatAttachment) []ChatAttachment {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]ChatAttachment, 0, len(in))
	for _, a := range in {
		key := a.Kind + "\x00" + a.Path + "\x00" + a.URL + "\x00" + a.Title
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, a)
	}
	return out
}

// hasVisibleText reports whether a string has any non-whitespace, non-isolate
// content. Kept here so text and media-only detection share one rule.
func hasVisibleText(s string) bool {
	return strings.IndexFunc(strings.Map(func(r rune) rune {
		if r == chatIsolateL || r == chatIsolateR {
			return -1
		}
		return r
	}, s), func(r rune) bool { return !unicode.IsSpace(r) }) >= 0
}

package gateway

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Console command center (§9.5) + rooms (§6.8).
//
// The console speaks on the message path as the reserved "operator" identity:
// POST /admin/messages goes through queue.Send, so an operator message gets the
// same guard, ordering, ack tracking and SSE wake-up as any other message. The
// v0.14.0 conversations design instead wrote rows straight into the store,
// which (among other things) meant an operator message never woke a single SSE
// subscriber. There are no new agent-facing endpoints: assistants receive room
// and operator messages through GET /messages like everything else.

// settingOperatorReadSeq is the console's inbox read watermark (§9.5).
const settingOperatorReadSeq = "operator_read_seq"

// roomSendIDPrefix labels server-minted message ids from the console and from
// server notices, so a row's origin is visible in exports and audits.
const (
	opSendIDPrefix     = "op"
	systemNoticePrefix = "sys"
)

// ---- operator sends ----

// handleAdminSend handles POST /admin/messages: the operator sends one message
// to a peer, a room alias (grp_*), or "*" (broadcast). `from` is always the
// reserved operator identity — the body cannot set it (readJSON rejects
// unknown fields, so a `from` key is a 400 rather than a silent override).
func (s *Server) handleAdminSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID               string `json:"id"`
		To               string `json:"to"`
		Kind             string `json:"kind"`
		Payload          string `json:"payload"`
		InReplyTo        string `json:"in_reply_to"`
		Decision         string `json:"decision"`
		Status           string `json:"status"`
		Op               string `json:"op"`
		Target           string `json:"target"`
		Detail           string `json:"detail"`
		ExpiresInSecs    int64  `json:"expires_in_secs"`
		RequiresApproval bool   `json:"requires_approval"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		req.ID = auth.NewMessageID(opSendIDPrefix)
	}
	send := &queue.SendRequest{
		ID: req.ID, To: req.To, From: auth.OperatorID, Kind: req.Kind,
		InReplyTo: req.InReplyTo, Payload: req.Payload, Decision: req.Decision,
		Status: req.Status, Op: req.Op, Target: req.Target, Detail: req.Detail,
		ExpiresInSecs: req.ExpiresInSecs, RequiresApproval: req.RequiresApproval,
	}
	seq, held, rootID, err := s.queue.Send(send, time.Now().Unix())
	if err != nil {
		s.sendRejection(w, err)
		return
	}
	if held {
		writeJSON(w, 202, map[string]any{"ok": true, "held": true, "seq": seq, "id": send.ID, "thread": rootID})
		return
	}
	// An operator message is the main "wake the fleet" path: without this the
	// message sat invisible to SSE subscribers until their stream reconnected.
	s.notifyStream()
	writeJSON(w, 200, map[string]any{"ok": true, "seq": seq, "id": send.ID, "thread": rootID})
}

// ---- rooms ----

// roomInfo is the console wire shape for one room.
type roomInfo struct {
	ID         string       `json:"id"`
	Name       string       `json:"name"`
	Note       string       `json:"note"`
	CreatedAt  int64        `json:"created_at"`
	Archived   bool         `json:"archived"`
	Count      int          `json:"count"`
	Members    []roomMember `json:"members"`
	LastSeq    int64        `json:"last_seq"`
	LastSender string       `json:"last_sender"`
	LastKind   string       `json:"last_kind"`
	LastAt     int64        `json:"last_at"`
	Preview    string       `json:"last_preview"`
	NotReady   []string     `json:"not_ready,omitempty"` // members below the room-aware prompt revision
	Unknown    []string     `json:"unknown_members,omitempty"`
}

// roomMember adds liveness to a member id for the console.
type roomMember struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	Online        bool   `json:"online"`
	PromptVersion int    `json:"prompt_version"`
	AddedAt       int64  `json:"added_at"`
	StartSeq      int64  `json:"start_seq"`
}

// handleAdminListRooms handles GET /admin/rooms.
func (s *Server) handleAdminListRooms(w http.ResponseWriter, r *http.Request) {
	includeArchived := r.URL.Query().Get("archived") == "1"
	rooms, err := s.st.ListRoomsWithStats(includeArchived)
	if err != nil {
		writeErr(w, 500, "rooms failed")
		return
	}
	out := make([]roomInfo, 0, len(rooms))
	for _, rs := range rooms {
		out = append(out, s.roomInfoOf(rs))
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "rooms": out, "max_members": s.cfg.MaxRoomMembers,
		"fuse_max_messages": s.cfg.RoomFuseMaxMessages, "fuse_window_secs": s.cfg.RoomFuseWindowSecs,
	})
}

// roomInfoOf decorates a room with member liveness and the prompt-version
// warning: a member that has not yet read the room rules would treat a room
// task as a private instruction, so the console shows it before you post.
func (s *Server) roomInfoOf(rs *store.RoomStats) roomInfo {
	info := roomInfo{
		ID: rs.ID, Name: rs.Name, Note: rs.Note, CreatedAt: rs.CreatedAt,
		Archived: rs.ArchivedAt != 0, Count: rs.Count,
		LastSeq: rs.LastSeq, LastSender: rs.LastSender, LastKind: rs.LastKind,
		LastAt: rs.LastAt, Preview: rs.LastPreview, Members: []roomMember{},
	}
	views, _ := s.presence.List(time.Now().Unix(), s.liveTransports())
	byID := map[string]*presence.PeerView{}
	for _, v := range views {
		byID[v.ID] = v
	}
	rows, _ := s.st.ListRoomMembers(rs.ID)
	added := map[string]int64{}
	start := map[string]int64{}
	for _, m := range rows {
		added[m.PeerID] = m.AddedAt
		start[m.PeerID] = m.StartSeq
	}
	for _, id := range rs.Members {
		rm := roomMember{ID: id, AddedAt: added[id], StartSeq: start[id]}
		if v := byID[id]; v != nil {
			rm.Status, rm.Online, rm.PromptVersion = v.Status, v.Online, v.PromptVersion
			if v.PromptVersion < prompts.RoomAwarePromptVersion {
				info.NotReady = append(info.NotReady, id)
			}
		} else {
			rm.Status = "unknown"
			info.Unknown = append(info.Unknown, id)
		}
		info.Members = append(info.Members, rm)
	}
	return info
}

// handleAdminCreateRoom handles POST /admin/rooms {id?, name?, note?, members[]}.
func (s *Server) handleAdminCreateRoom(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Note    string   `json:"note"`
		Members []string `json:"members"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	id, err := normalizeRoomID(req.ID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if existing, _ := s.st.GetRoom(id); existing != nil {
		writeErr(w, 409, "room "+id+" already exists")
		return
	}
	if s.cfg.MaxRoomMembers > 0 && len(req.Members) > s.cfg.MaxRoomMembers {
		writeErr(w, 400, fmt.Sprintf("max %d members per room", s.cfg.MaxRoomMembers))
		return
	}
	now := time.Now().Unix()
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = id
	}
	room := &store.Room{ID: id, Name: name, Note: strings.TrimSpace(req.Note), CreatedAt: now}
	if err := s.st.CreateRoom(room); err != nil {
		writeErr(w, 500, "create room failed")
		return
	}
	// start_seq = the current max: members see what happens from now on. A
	// brand-new room has no history, so this only matters for adds later.
	startSeq, _ := s.st.MaxSeq()
	for _, peerID := range dedupeIDs(req.Members) {
		if p, _ := s.st.GetPeer(peerID); p == nil {
			writeErr(w, 400, "unknown peer "+peerID)
			return
		}
		if err := s.st.AddRoomMember(id, peerID, startSeq, now); err != nil {
			writeErr(w, 500, "add member failed")
			return
		}
	}
	_ = s.st.AppendAudit("admin", "room.created",
		fmt.Sprintf("room=%s members=%d", id, len(req.Members)), now)
	s.roomNotice(id, fmt.Sprintf("群聊 %s 已创建（成员：%s）。群里发言用 to=%s；只回操作者用 to=%s。",
		id, strings.Join(req.Members, "、"), id, auth.OperatorID))
	rs, _ := s.st.ListRoomsWithStats(false)
	info := roomInfo{ID: room.ID, Name: room.Name, Note: room.Note, CreatedAt: room.CreatedAt}
	for _, one := range rs {
		if one.ID == id {
			info = s.roomInfoOf(one)
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "room": info})
}

// handleAdminPatchRoom handles PATCH /admin/rooms/{id} {name?, note?, archived?}.
func (s *Server) handleAdminPatchRoom(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	room, err := s.st.GetRoom(id)
	if err != nil || room == nil {
		writeErr(w, 404, "unknown room")
		return
	}
	var req struct {
		Name     *string `json:"name"`
		Note     *string `json:"note"`
		Archived *bool   `json:"archived"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	name, note := room.Name, room.Note
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Note != nil {
		note = strings.TrimSpace(*req.Note)
	}
	if err := s.st.UpdateRoom(id, name, note); err != nil {
		writeErr(w, 500, "update failed")
		return
	}
	if req.Archived != nil {
		ts := int64(0)
		if *req.Archived {
			ts = time.Now().Unix()
		}
		if err := s.st.SetRoomArchived(id, ts); err != nil {
			writeErr(w, 500, "archive failed")
			return
		}
		_ = s.st.AppendAudit("admin", "room.archived",
			fmt.Sprintf("room=%s archived=%v", id, *req.Archived), time.Now().Unix())
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminAddRoomMember handles POST /admin/rooms/{id}/members {peer_id}.
func (s *Server) handleAdminAddRoomMember(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	room, err := s.st.GetRoom(id)
	if err != nil || room == nil {
		writeErr(w, 404, "unknown room")
		return
	}
	if p, _ := s.st.GetPeer(req.PeerID); p == nil {
		writeErr(w, 400, "unknown peer "+req.PeerID)
		return
	}
	members, _ := s.st.ListRoomMembers(id)
	if s.cfg.MaxRoomMembers > 0 && len(members) >= s.cfg.MaxRoomMembers {
		writeErr(w, 400, fmt.Sprintf("room is at its %d member cap", s.cfg.MaxRoomMembers))
		return
	}
	already, _ := s.st.IsRoomMember(id, req.PeerID)
	now := time.Now().Unix()
	startSeq, _ := s.st.MaxSeq()
	if err := s.st.AddRoomMember(id, req.PeerID, startSeq, now); err != nil {
		writeErr(w, 500, "add member failed")
		return
	}
	if !already {
		_ = s.st.AppendAudit("admin", "room.member_added",
			fmt.Sprintf("room=%s peer=%s", id, req.PeerID), now)
		s.roomNotice(id, fmt.Sprintf("%s 加入群聊（只看加入之后的消息）。", req.PeerID))
	}
	writeJSON(w, 200, map[string]any{"ok": true, "already": already})
}

// handleAdminRemoveRoomMember handles DELETE /admin/rooms/{id}/members/{peer}.
func (s *Server) handleAdminRemoveRoomMember(w http.ResponseWriter, r *http.Request) {
	id, peerID := r.PathValue("id"), r.PathValue("peer")
	room, err := s.st.GetRoom(id)
	if err != nil {
		writeErr(w, 500, "room lookup failed")
		return
	}
	if room == nil {
		writeErr(w, 404, "unknown room")
		return
	}
	member, _ := s.st.IsRoomMember(id, peerID)
	if err := s.st.RemoveRoomMember(id, peerID); err != nil {
		writeErr(w, 500, "remove member failed")
		return
	}
	if member {
		now := time.Now().Unix()
		_ = s.st.AppendAudit("admin", "room.member_removed",
			fmt.Sprintf("room=%s peer=%s", id, peerID), now)
		s.roomNotice(id, fmt.Sprintf("%s 已移出群聊，之后的消息不再发给它。", peerID))
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// roomNotice posts a system message into a room (roster changes). It rides the
// normal send path so it obeys the room fuse and wakes SSE subscribers; the
// guard exempts the server's own identities from the membership check.
func (s *Server) roomNotice(roomID, text string) {
	_, _, _, err := s.queue.Send(&queue.SendRequest{
		ID: auth.NewMessageID(systemNoticePrefix), To: roomID, From: auth.SystemID,
		Kind: "system", Payload: text,
	}, time.Now().Unix())
	if err == nil {
		s.notifyStream()
	}
}

// normalizeRoomID accepts "ops", "grp_ops" or "Grp_Ops" and returns a valid
// lowercase grp_ alias, so the console can be lenient without letting an
// invalid alias into the database.
func normalizeRoomID(in string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(in))
	if id == "" {
		return "", fmt.Errorf("room id is required (short name, e.g. ops)")
	}
	if !strings.HasPrefix(id, "grp_") {
		id = "grp_" + id
	}
	id = strings.NewReplacer(" ", "-", ".", "-", "/", "-").Replace(id)
	if !auth.ValidRoomID(id) {
		return "", fmt.Errorf("room id must match grp_<slug>: lowercase letters, digits, - and _")
	}
	return id, nil
}

func dedupeIDs(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ---- operator inbox ----

// handleAdminInbox handles GET /admin/inbox: replies addressed to the operator.
func (s *Server) handleAdminInbox(w http.ResponseWriter, r *http.Request) {
	p := parsePage(r)
	items, total, err := s.st.InboxMessages(p.Size, p.offset())
	if err != nil {
		writeErr(w, 500, "inbox failed")
		return
	}
	watermark := s.operatorReadSeq()
	out := make([]map[string]any, 0, len(items))
	for _, m := range items {
		out = append(out, map[string]any{
			"seq": m.Seq, "id": m.ID, "from": m.Sender, "to": m.Recipient,
			"kind": m.Kind, "in_reply_to": m.InReplyTo, "thread": m.RootID,
			"payload": m.Payload, "created_at": m.CreatedAt,
			"status": m.Status, "decision": m.Decision,
			"unread": m.Seq > watermark,
		})
	}
	unread, _ := s.st.CountInboxSince(watermark)
	writeJSON(w, 200, withMeta(map[string]any{
		"ok": true, "items": out, "read_seq": watermark, "unread": unread,
	}, p, total))
}

// handleAdminInboxRead handles POST /admin/inbox/read {seq}: move the read
// watermark forward (never backwards, so a stale tab cannot unread messages).
func (s *Server) handleAdminInboxRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seq int64 `json:"seq"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	cur := s.operatorReadSeq()
	if req.Seq < cur {
		req.Seq = cur
	}
	if err := s.st.SetSetting(settingOperatorReadSeq, strconv.FormatInt(req.Seq, 10), time.Now().Unix()); err != nil {
		writeErr(w, 500, "read marker failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "read_seq": req.Seq})
}

// operatorReadSeq returns the stored inbox watermark (0 when never read).
func (s *Server) operatorReadSeq() int64 {
	v, err := s.st.GetSetting(settingOperatorReadSeq)
	if err != nil || v == "" {
		return 0
	}
	seq, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return seq
}

package gateway

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/web/views"
)

// The 指挥台 page (§9.5): templ + htmx over the admin JSON APIs above. The page
// is a thin, server-rendered view — every action calls the same helpers the
// JSON API uses (operatorSend/createRoom/addRoomMember/removeRoomMember), so
// there is one implementation of the rules and the console cannot drift from
// the API. Fragments are swapped by htmx; errors render as toasts inside the
// swapped region rather than HTTP error pages, which is what a form user needs.

const commandInboxLimit = 50

// ---- page + fragments ----

// handleCommandPage handles GET /admin/command[?room=grp_x].
func (s *Server) handleCommandPage(w http.ResponseWriter, r *http.Request) {
	views.CommandPage(s.commandData(r.URL.Query().Get("room"))).Render(r.Context(), w)
}

// handleCommandRoomsFragment handles GET /admin/command/rooms (htmx swap).
func (s *Server) handleCommandRoomsFragment(w http.ResponseWriter, r *http.Request) {
	views.RoomCardsFragment(s.commandData("")).Render(r.Context(), w)
}

// handleCommandRoomFragment handles GET /admin/command/room/{id} (htmx swap).
func (s *Server) handleCommandRoomFragment(w http.ResponseWriter, r *http.Request) {
	data := s.commandData(r.PathValue("id"))
	if data.Room == nil {
		data.Error = "群聊 " + r.PathValue("id") + " 不存在或已删除"
	}
	views.RoomPaneFragment(data).Render(r.Context(), w)
}

// handleCommandInboxFragment handles GET /admin/command/inbox (htmx swap).
func (s *Server) handleCommandInboxFragment(w http.ResponseWriter, r *http.Request) {
	views.InboxPanelFragment(s.commandData("")).Render(r.Context(), w)
}

// handleCommandSendForm handles POST /admin/command/send {to,kind,payload}: the
// composer for a peer, a room alias or "*".
func (s *Server) handleCommandSendForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		_ = views.SendResult("", "表单解析失败").Render(ctx, w)
		return
	}
	to := strings.TrimSpace(r.FormValue("to"))
	kind := strings.TrimSpace(r.FormValue("kind"))
	payload := strings.TrimSpace(r.FormValue("payload"))
	if payload == "" {
		_ = views.SendResult("", "内容不能为空").Render(ctx, w)
		return
	}
	seq, held, rootID, err := s.operatorSend(&queue.SendRequest{To: to, Kind: kind, Payload: payload})
	if err != nil {
		// A room pane is the natural place for a room error, so the user keeps
		// the context they were typing in.
		if strings.HasPrefix(to, "grp_") {
			data := s.commandData(to)
			data.Error = err.Error()
			_ = views.RoomPaneFragment(data).Render(ctx, w)
			return
		}
		_ = views.SendResult("", err.Error()).Render(ctx, w)
		return
	}
	if strings.HasPrefix(to, "grp_") {
		data := s.commandData(to)
		data.Notice = fmt.Sprintf("已发到 %s（seq %d）", to, seq)
		if held {
			data.Notice = fmt.Sprintf("已提交待审批（seq %d）", seq)
		}
		_ = views.RoomPaneFragment(data).Render(ctx, w)
		return
	}
	notice := fmt.Sprintf("已发送给 %s（seq %d，线程 %s）", displayTarget(to), seq, rootID)
	if held {
		notice = fmt.Sprintf("已提交，等待审批后送达（seq %d）", seq)
	}
	_ = views.SendResult(notice, "").Render(ctx, w)
}

// handleCommandCreateRoomForm handles POST /admin/command/rooms; returns the
// refreshed room list so the new room shows up where the form was.
func (s *Server) handleCommandCreateRoomForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil {
		data := s.commandData("")
		data.Error = "表单解析失败"
		_ = views.RoomCardsFragment(data).Render(ctx, w)
		return
	}
	members := []string{}
	for _, m := range r.Form["members"] {
		members = append(members, strings.TrimSpace(m))
	}
	info, err := s.createRoom(r.FormValue("id"), r.FormValue("name"), "", members)
	data := s.commandData("")
	if err != nil {
		data.Error = err.Error()
		_ = views.RoomCardsFragment(data).Render(ctx, w)
		return
	}
	data.Notice = fmt.Sprintf("群聊 %s 已创建（%d 名成员）", info.ID, len(info.Members))
	_ = views.RoomCardsFragment(data).Render(ctx, w)
}

// handleCommandRoomMemberForm handles POST /admin/command/rooms/{id}/members
// {action: add|remove, peer_id}: membership changes plus the notice they post.
func (s *Server) handleCommandRoomMemberForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	roomID := r.PathValue("id")
	if err := r.ParseForm(); err != nil {
		data := s.commandData(roomID)
		data.Error = "表单解析失败"
		_ = views.RoomPaneFragment(data).Render(ctx, w)
		return
	}
	peerID := strings.TrimSpace(r.FormValue("peer_id"))
	var err error
	action := r.FormValue("action")
	switch action {
	case "add":
		var already bool
		already, err = s.addRoomMember(roomID, peerID)
		if err == nil {
			if already {
				err = fmt.Errorf("%s 本来就在群里", peerID)
			}
		}
	case "remove":
		err = s.removeRoomMember(roomID, peerID)
	default:
		err = fmt.Errorf("未知操作 %q", action)
	}
	data := s.commandData(roomID)
	if err != nil {
		data.Error = err.Error()
	} else if action == "add" {
		data.Notice = peerID + " 已加入群聊（只看加入之后的消息）"
	} else {
		data.Notice = peerID + " 已移出群聊"
	}
	_ = views.RoomPaneFragment(data).Render(ctx, w)
}

// handleCommandInboxReadForm handles POST /admin/command/inbox/read: move the
// console's read watermark to the newest inbox message.
func (s *Server) handleCommandInboxReadForm(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, _, err := s.st.InboxMessages(1, 0)
	data := s.commandData("")
	if err != nil {
		data.Error = "读取收件箱失败"
	} else if len(items) > 0 {
		if err := s.st.SetSetting(settingOperatorReadSeq, fmt.Sprintf("%d", items[0].Seq), time.Now().Unix()); err != nil {
			data.Error = "标记已读失败"
		} else {
			data.Notice = "已全部标记为已读"
			data = s.commandData("") // reload so the badge is gone
			data.Notice = "已全部标记为已读"
		}
	} else {
		data.Notice = "收件箱是空的"
	}
	_ = views.InboxPanelFragment(data).Render(ctx, w)
}

// ---- view-model builders ----

// commandData assembles the whole 指挥台 view (inbox + rooms + peers), plus the
// selected room pane when roomID is given.
func (s *Server) commandData(roomID string) views.CommandData {
	peers := s.commandPeers()
	byID := map[string]views.PeerOption{}
	for _, p := range peers {
		byID[p.ID] = p
	}
	watermark := s.operatorReadSeq()
	d := views.CommandData{
		Peers:      peers,
		ReadSeq:    watermark,
		MaxMembers: s.cfg.MaxRoomMembers,
		FuseMax:    s.cfg.RoomFuseMaxMessages,
		FuseWindow: s.cfg.RoomFuseWindowSecs,
	}
	if items, _, err := s.st.InboxMessages(commandInboxLimit, 0); err == nil {
		for _, m := range items {
			d.Inbox = append(d.Inbox, views.InboxItem{
				Seq: m.Seq, From: m.Sender, Kind: m.Kind, Payload: m.Payload,
				At: m.CreatedAt, Unread: m.Seq > watermark, Thread: m.RootID,
			})
		}
	}
	if unread, err := s.st.CountInboxSince(watermark); err == nil {
		d.Unread = int64(unread)
	}
	if rooms, err := s.st.ListRoomsWithStats(false); err == nil {
		for _, rs := range rooms {
			card := views.RoomCard{
				ID: rs.ID, Name: rs.Name, Members: rs.Members, Count: rs.Count,
				LastSender: rs.LastSender, LastPreview: rs.LastPreview, LastAt: rs.LastAt,
			}
			for _, memberID := range rs.Members {
				if p, ok := byID[memberID]; !ok || p.PromptVersion < prompts.RoomAwarePromptVersion {
					card.NotReady = append(card.NotReady, memberID)
				}
			}
			d.Rooms = append(d.Rooms, card)
		}
	}
	if roomID != "" {
		d.Room = s.commandRoom(roomID, byID)
	}
	return d
}

// commandRoom builds the room pane: members with liveness, the timeline with
// per-member acks, and the peers that are not in the room yet.
func (s *Server) commandRoom(roomID string, byID map[string]views.PeerOption) *views.RoomViewData {
	room, err := s.st.GetRoom(roomID)
	if err != nil || room == nil {
		return nil
	}
	view := &views.RoomViewData{
		ID: room.ID, Name: room.Name,
		MaxMembers: s.cfg.MaxRoomMembers,
		FuseWindow: s.cfg.RoomFuseWindowSecs,
		FuseMax:    s.cfg.RoomFuseMaxMessages,
	}
	memberSet := map[string]bool{}
	if members, err := s.st.ListRoomMembers(roomID); err == nil {
		for _, m := range members {
			memberSet[m.PeerID] = true
			opt, ok := byID[m.PeerID]
			if !ok {
				opt = views.PeerOption{ID: m.PeerID, Status: "unknown"}
			}
			view.Members = append(view.Members, opt)
			if opt.PromptVersion < prompts.RoomAwarePromptVersion {
				view.NotReady = append(view.NotReady, m.PeerID)
			}
		}
	}
	for _, p := range byID {
		if !memberSet[p.ID] {
			view.NonMembers = append(view.NonMembers, p)
		}
	}
	msgs, err := s.st.ThreadMessages(roomID, 200)
	if err != nil {
		return view
	}
	seqs := make([]int64, 0, len(msgs))
	for _, m := range msgs {
		seqs = append(seqs, m.Seq)
	}
	acks, _ := s.st.AcksForSeqs(seqs)
	for _, m := range msgs {
		row := views.RoomMessage{
			Seq: m.Seq, From: m.Sender, Kind: m.Kind, Payload: m.Payload, At: m.CreatedAt,
		}
		if who := acks[m.Seq]; len(who) > 0 {
			row.AckedBy = who
		}
		view.Messages = append(view.Messages, row)
	}
	return view
}

// commandPeers lists peers for the pickers, newest state first.
func (s *Server) commandPeers() []views.PeerOption {
	list, err := s.presence.List(time.Now().Unix(), s.liveTransports())
	if err != nil {
		return nil
	}
	out := make([]views.PeerOption, 0, len(list))
	for _, v := range list {
		out = append(out, views.PeerOption{
			ID: v.ID, Status: v.Status, Online: v.Online, PromptVersion: v.PromptVersion,
		})
	}
	return out
}

// displayTarget renders a recipient for the send toast.
func displayTarget(to string) string {
	switch {
	case to == "*":
		return "全体成员"
	case to == auth.OperatorID:
		return "控制台"
	default:
		return to
	}
}

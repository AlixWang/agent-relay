package gateway

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/web/views"
)

// handleConversationsPage renders the conversations page with Templ
func (s *Server) handleConversationsPage(w http.ResponseWriter, r *http.Request) {
	convs, err := s.st.ListConversations(50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Convert store.Conversation to views.Conversation
	viewConvs := make([]views.Conversation, len(convs))
	for i, c := range convs {
		// Get member count
		memberCount, _ := s.st.CountConversationMembers(c.ID)
		// Get latest seq
		latestSeq, _ := s.st.ConversationMaxSeq(c.ID)

		viewConvs[i] = views.Conversation{
			ID:          c.ID,
			Type:        c.Type,
			Title:       c.Title,
			CreatedBy:   c.CreatedBy,
			CreatedAt:   c.CreatedAt,
			MemberCount: memberCount,
			LatestSeq:   latestSeq,
			AgentStreak: c.AgentStreak,
		}
	}

	views.ConversationsPage(viewConvs).Render(r.Context(), w)
}

// handleNewConversationModal renders the create conversation modal
func (s *Server) handleNewConversationModal(w http.ResponseWriter, r *http.Request) {
	peers, err := s.st.ListPeers()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Convert to view model
	viewPeers := make([]views.Peer, len(peers))
	for i, p := range peers {
		viewPeers[i] = views.Peer{
			ID:     p.ID,
			Status: p.Status,
		}
	}

	views.CreateConversationModal(viewPeers).Render(r.Context(), w)
}

// handleCreateConversationHTMX handles conversation creation from HTMX form
func (s *Server) handleCreateConversationHTMX(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}

	convType := r.FormValue("type")
	title := r.FormValue("title")
	memberIDs := r.Form["member_ids"]

	if title == "" {
		http.Error(w, "title required", 400)
		return
	}
	if len(memberIDs) == 0 {
		http.Error(w, "at least one member required", 400)
		return
	}

	// Validate member count
	if len(memberIDs) > s.cfg.ConvMaxMembers {
		writeErr(w, 400, "too many members")
		return
	}

	// Validate all members exist and are active
	for _, mid := range memberIDs {
		peer, err := s.st.GetPeer(mid)
		if err != nil || peer == nil || peer.Status != "active" {
			writeErr(w, 400, "invalid or inactive member: "+mid)
			return
		}
	}

	// Create conversation
	convID := "conv-" + generateID()
	now := nowUnix()
	conv := &store.Conversation{
		ID:          convID,
		Type:        convType,
		Title:       title,
		CreatedBy:   "admin", // Admin created via UI
		CreatedAt:   now,
		ArchivedAt:  0,
		AgentStreak: 0,
	}

	if err := s.st.CreateConversation(conv); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Add members
	for i, mid := range memberIDs {
		role := "member"
		if i == 0 {
			role = "creator"
		}
		if err := s.st.AddConversationMember(convID, mid, role, 0); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}

	// Audit log
	s.audit("conversation.created", map[string]any{
		"conv_id": convID,
		"type":    convType,
		"title":   title,
		"members": memberIDs,
	})

	// Return the new conversation card
	memberCount := len(memberIDs)
	viewConv := views.Conversation{
		ID:          convID,
		Type:        convType,
		Title:       title,
		CreatedBy:   "admin",
		CreatedAt:   now,
		MemberCount: memberCount,
		LatestSeq:   0,
		AgentStreak: 0,
	}

	views.ConversationCard(viewConv).Render(r.Context(), w)
}

// handleConversationDrawer renders the conversation detail drawer
func (s *Server) handleConversationDrawer(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	if convID == "" {
		http.Error(w, "missing conv_id", 400)
		return
	}

	// Get conversation
	conv, err := s.st.GetConversation(convID)
	if err != nil || conv == nil {
		http.Error(w, "conversation not found", 404)
		return
	}

	// Get member count
	memberCount, _ := s.st.CountConversationMembers(convID)

	// Get messages
	messages, err := s.st.VisibleTo("user", 0, 50) // Use "user" to see all
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Filter messages for this conversation
	var convMessages []views.Message
	for _, m := range messages {
		if m.ConvID == convID {
			convMessages = append(convMessages, views.Message{
				ID:        m.ID,
				Sender:    m.Sender,
				Payload:   m.Payload,
				CreatedAt: m.CreatedAt,
			})
		}
	}

	// Get latest seq
	latestSeq, _ := s.st.ConversationMaxSeq(convID)

	viewConv := views.ConversationDetail{
		ID:          conv.ID,
		Type:        conv.Type,
		Title:       conv.Title,
		MemberCount: memberCount,
		LatestSeq:   latestSeq,
		Messages:    convMessages,
	}

	views.ConversationDrawer(viewConv).Render(r.Context(), w)
}

// handleConversationMessages returns just the message list
func (s *Server) handleConversationMessages(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	if convID == "" {
		http.Error(w, "missing conv_id", 400)
		return
	}

	// Get messages
	messages, err := s.st.VisibleTo("user", 0, 50)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Filter for this conversation
	var convMessages []views.Message
	for _, m := range messages {
		if m.ConvID == convID {
			convMessages = append(convMessages, views.Message{
				ID:        m.ID,
				Sender:    m.Sender,
				Payload:   m.Payload,
				CreatedAt: m.CreatedAt,
			})
		}
	}

	views.MessageList(convMessages).Render(r.Context(), w)
}

// handleSendMessageHTMX handles sending a message from HTMX form
func (s *Server) handleSendMessageHTMX(w http.ResponseWriter, r *http.Request) {
	convID := r.PathValue("id")
	if convID == "" {
		http.Error(w, "missing conv_id", 400)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}

	payload := r.FormValue("payload")
	if payload == "" {
		http.Error(w, "payload required", 400)
		return
	}

	// Extract @mentions
	mentionRe := regexp.MustCompile(`@([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})`)
	matches := mentionRe.FindAllStringSubmatch(payload, -1)
	mentions := make([]string, 0)
	seen := make(map[string]bool)
	for _, m := range matches {
		if len(m) > 1 && !seen[m[1]] {
			mentions = append(mentions, m[1])
			seen[m[1]] = true
		}
	}

	mentionsJSON := ""
	if len(mentions) > 0 {
		b, _ := json.Marshal(mentions)
		mentionsJSON = string(b)
	}

	// Insert message
	now := nowUnix()
	msgID := "msg-" + generateID()
	msg := &store.Message{
		ID:            msgID,
		Sender:        "user",
		Recipient:     "conv:" + convID,
		Kind:          "chat",
		RootID:        convID,
		Payload:       payload,
		CreatedAt:     now,
		ConvID:        convID,
		Mentions:      mentionsJSON,
		ApprovalState: "n/a",
	}

	if _, err := s.st.InsertMessage(msg); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Update conversation streak (reset to 0 for user)
	s.guard.UpdateConversationStreak(convID, "user")

	// Audit log
	s.audit("message.sent_as_user", map[string]any{
		"conv_id":  convID,
		"msg_id":   msgID,
		"mentions": mentions,
	})

	// Return the new message item
	viewMsg := views.Message{
		ID:        msgID,
		Sender:    "user",
		Payload:   payload,
		CreatedAt: now,
	}

	views.MessageItem(viewMsg).Render(r.Context(), w)
}

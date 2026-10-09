// Package gateway - Conversation APIs (§v12).
// Handles group chat and private conversation management.
package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/google/uuid"
)

// ---- Assistant APIs (authenticated via Bearer token) ----

// handleCreateConversation handles POST /conversations.
// Creates a new conversation and adds the creator plus specified members.
func (s *Server) handleCreateConversation(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	var req struct {
		Type      string   `json:"type"`       // "dm" or "group"
		Title     string   `json:"title"`      // conversation title
		MemberIDs []string `json:"member_ids"` // initial members (excluding creator)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON")
		return
	}

	// Validate type
	if req.Type == "" {
		req.Type = "group"
	}
	if req.Type != "dm" && req.Type != "group" {
		writeErr(w, http.StatusBadRequest, "type must be 'dm' or 'group'")
		return
	}

	// Check member count limit
	totalMembers := len(req.MemberIDs) + 1 // +1 for creator
	if totalMembers > s.cfg.ConvMaxMembers {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("max %d members allowed", s.cfg.ConvMaxMembers))
		return
	}
	if totalMembers < 2 {
		writeErr(w, http.StatusBadRequest, "at least 2 members required (creator + 1 other)")
		return
	}

	// Rate limit: check conversation creation per hour
	// TODO: Implement per-peer rate limiting for conversation creation
	// For now, we'll skip this and add it in a future iteration

	// Validate that all member IDs are active peers
	for _, memberID := range req.MemberIDs {
		p, err := s.st.GetPeer(memberID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if p == nil || p.Status != "active" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("member %s not found or not active", memberID))
			return
		}
	}

	// Create conversation
	now := time.Now().Unix()
	convID := "conv-" + uuid.New().String()
	conv := &store.Conversation{
		ID:          convID,
		Type:        req.Type,
		Title:       req.Title,
		CreatedBy:   peer.ID,
		CreatedAt:   now,
		ArchivedAt:  0,
		AgentStreak: 0,
	}

	if err := s.st.CreateConversation(conv); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create conversation")
		return
	}

	// Add creator as first member
	if err := s.st.AddConversationMember(convID, peer.ID, "creator", 0); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to add creator")
		return
	}

	// Add other members
	for _, memberID := range req.MemberIDs {
		if err := s.st.AddConversationMember(convID, memberID, "member", 0); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to add member")
			return
		}
	}

	// Audit log
	_ = s.st.AppendAudit(peer.ID, "conversation.created",
		fmt.Sprintf("conv_id=%s type=%s members=%d", convID, req.Type, totalMembers), now)

	writeJSON(w, map[string]any{
		"ok":      true,
		"conv_id": convID,
	})
}

// handleListConversations handles GET /conversations.
// Returns conversations the authenticated peer is a member of.
func (s *Server) handleListConversations(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	conversations, err := s.st.ListConversationsForPeer(peer.ID, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}

	// Enrich with member count and latest_seq
	type convResp struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Title       string `json:"title"`
		CreatedBy   string `json:"created_by"`
		CreatedAt   int64  `json:"created_at"`
		MemberCount int    `json:"member_count"`
		LatestSeq   int64  `json:"latest_seq"`
	}
	var resp []convResp
	for _, c := range conversations {
		count, _ := s.st.CountConversationMembers(c.ID)
		latestSeq, _ := s.st.ConversationMaxSeq(c.ID)
		resp = append(resp, convResp{
			ID:          c.ID,
			Type:        c.Type,
			Title:       c.Title,
			CreatedBy:   c.CreatedBy,
			CreatedAt:   c.CreatedAt,
			MemberCount: count,
			LatestSeq:   latestSeq,
		})
	}

	writeJSON(w, map[string]any{
		"ok":            true,
		"conversations": resp,
	})
}

// handleGetConversationMessages handles GET /conversations/{id}/messages.
// Returns message history for a conversation.
func (s *Server) handleGetConversationMessages(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	convID := strings.TrimPrefix(r.URL.Path, "/conversations/")
	convID = strings.TrimSuffix(convID, "/messages")

	// Check membership
	isMember, err := s.st.IsConversationMember(convID, peer.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !isMember {
		writeErr(w, http.StatusForbidden, "not a member of this conversation")
		return
	}

	// Parse limit and before_seq
	limit := 30
	beforeSeq := int64(0)
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}
	if b := r.URL.Query().Get("before_seq"); b != "" {
		fmt.Sscanf(b, "%d", &beforeSeq)
	}

	// Get conversation messages (simple query for now)
	// TODO: Implement pagination with before_seq
	// For now, just get all messages
	allMessages, err := s.st.ThreadMessages(convID, 1000) // Use ThreadMessages as temporary solution
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}

	// Filter to conversation messages only
	var messages []*store.Message
	for _, m := range allMessages {
		if m.ConvID == convID {
			messages = append(messages, m)
		}
	}

	// Get latest seq
	latestSeq, _ := s.st.ConversationMaxSeq(convID)

	writeJSON(w, map[string]any{
		"ok":         true,
		"messages":   messages,
		"latest_seq": latestSeq,
	})
}

// handleLeaveConversation handles POST /conversations/{id}/leave.
// Removes the authenticated peer from the conversation.
func (s *Server) handleLeaveConversation(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	convID := strings.TrimPrefix(r.URL.Path, "/conversations/")
	convID = strings.TrimSuffix(convID, "/leave")

	// Check membership
	isMember, err := s.st.IsConversationMember(convID, peer.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	if !isMember {
		writeErr(w, http.StatusNotFound, "not a member of this conversation")
		return
	}

	// Mark as left
	now := time.Now().Unix()
	if err := s.st.RemoveConversationMember(convID, peer.ID, now); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to leave conversation")
		return
	}

	// Audit log
	_ = s.st.AppendAudit(peer.ID, "conversation.left", "conv_id="+convID, now)

	writeJSON(w, map[string]any{"ok": true})
}

// ---- Admin APIs (authenticated via admin session) ----

// handleAdminListConversations handles GET /admin/conversations.
// Returns all conversations with pagination.
func (s *Server) handleAdminListConversations(w http.ResponseWriter, r *http.Request) {
	// Parse pagination
	page := 1
	pageSize := 20
	if p := r.URL.Query().Get("page"); p != "" {
		fmt.Sscanf(p, "%d", &page)
	}
	if ps := r.URL.Query().Get("page_size"); ps != "" {
		fmt.Sscanf(ps, "%d", &pageSize)
	}
	if pageSize > 100 {
		pageSize = 100
	}

	// Get conversations (simple implementation without pagination for now)
	conversations, err := s.st.ListConversations(pageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}

	// Enrich with member count
	type convResp struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		Title       string `json:"title"`
		CreatedBy   string `json:"created_by"`
		CreatedAt   int64  `json:"created_at"`
		MemberCount int    `json:"member_count"`
		AgentStreak int    `json:"agent_streak"`
	}
	var resp []convResp
	for _, c := range conversations {
		count, _ := s.st.CountConversationMembers(c.ID)
		resp = append(resp, convResp{
			ID:          c.ID,
			Type:        c.Type,
			Title:       c.Title,
			CreatedBy:   c.CreatedBy,
			CreatedAt:   c.CreatedAt,
			MemberCount: count,
			AgentStreak: c.AgentStreak,
		})
	}

	writeJSON(w, map[string]any{
		"ok":            true,
		"conversations": resp,
		"page":          page,
		"page_size":     pageSize,
	})
}

// handleAdminCreateConversation handles POST /admin/conversations.
// Admin version of conversation creation.
func (s *Server) handleAdminCreateConversation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Type      string   `json:"type"`
		Title     string   `json:"title"`
		MemberIDs []string `json:"member_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON")
		return
	}

	// Validate
	if req.Type == "" {
		req.Type = "group"
	}
	if req.Type != "dm" && req.Type != "group" {
		writeErr(w, http.StatusBadRequest, "type must be 'dm' or 'group'")
		return
	}
	if len(req.MemberIDs) < 1 {
		writeErr(w, http.StatusBadRequest, "at least 1 member required")
		return
	}
	if len(req.MemberIDs) > s.cfg.ConvMaxMembers {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("max %d members allowed", s.cfg.ConvMaxMembers))
		return
	}

	// Validate members
	for _, memberID := range req.MemberIDs {
		p, err := s.st.GetPeer(memberID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if p == nil || p.Status != "active" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("member %s not found or not active", memberID))
			return
		}
	}

	// Create conversation
	now := time.Now().Unix()
	convID := "conv-" + uuid.New().String()
	conv := &store.Conversation{
		ID:          convID,
		Type:        req.Type,
		Title:       req.Title,
		CreatedBy:   "admin",
		CreatedAt:   now,
		ArchivedAt:  0,
		AgentStreak: 0,
	}

	if err := s.st.CreateConversation(conv); err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to create conversation")
		return
	}

	// Add members
	for _, memberID := range req.MemberIDs {
		role := "member"
		if memberID == req.MemberIDs[0] {
			role = "creator" // First member is creator
		}
		if err := s.st.AddConversationMember(convID, memberID, role, 0); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to add member")
			return
		}
	}

	// Audit log
	_ = s.st.AppendAudit("admin", "conversation.created",
		fmt.Sprintf("conv_id=%s type=%s members=%d", convID, req.Type, len(req.MemberIDs)), now)

	writeJSON(w, map[string]any{
		"ok":      true,
		"conv_id": convID,
	})
}

// handleAdminManageMembers handles PATCH /admin/conversations/{id}/members.
// Add or remove members from a conversation.
func (s *Server) handleAdminManageMembers(w http.ResponseWriter, r *http.Request) {
	convID := strings.TrimPrefix(r.URL.Path, "/admin/conversations/")
	convID = strings.TrimSuffix(convID, "/members")

	var req struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON")
		return
	}

	now := time.Now().Unix()

	// Add members
	for _, memberID := range req.Add {
		// Check if peer exists and is active
		p, err := s.st.GetPeer(memberID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "database error")
			return
		}
		if p == nil || p.Status != "active" {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("member %s not found or not active", memberID))
			return
		}

		// Get current max seq for joined_seq
		maxSeq, _ := s.st.ConversationMaxSeq(convID)

		if err := s.st.AddConversationMember(convID, memberID, "member", maxSeq); err != nil {
			// Ignore duplicate errors
			if !strings.Contains(err.Error(), "UNIQUE") {
				writeErr(w, http.StatusInternalServerError, "failed to add member")
				return
			}
		}
	}

	// Remove members
	for _, memberID := range req.Remove {
		if err := s.st.RemoveConversationMember(convID, memberID, now); err != nil {
			writeErr(w, http.StatusInternalServerError, "failed to remove member")
			return
		}
	}

	// Audit log
	if len(req.Add) > 0 || len(req.Remove) > 0 {
		detail := fmt.Sprintf("conv_id=%s added=%d removed=%d", convID, len(req.Add), len(req.Remove))
		_ = s.st.AppendAudit("admin", "conversation.members_changed", detail, now)
	}

	writeJSON(w, map[string]any{"ok": true})
}

// handleAdminSendMessage handles POST /admin/conversations/{id}/messages.
// Send a message as "user" (admin) to a conversation.
func (s *Server) handleAdminSendMessage(w http.ResponseWriter, r *http.Request) {
	convID := strings.TrimPrefix(r.URL.Path, "/admin/conversations/")
	convID = strings.TrimSuffix(convID, "/messages")

	var req struct {
		Payload  string   `json:"payload"`
		Mentions []string `json:"mentions"` // optional @mentions
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad JSON")
		return
	}

	if strings.TrimSpace(req.Payload) == "" {
		writeErr(w, http.StatusBadRequest, "payload is required")
		return
	}

	// Parse @mentions from payload if not provided
	if len(req.Mentions) == 0 {
		req.Mentions = extractMentions(req.Payload)
	}

	// Create message
	now := time.Now().Unix()
	msgID := fmt.Sprintf("admin-msg-%d", now)

	// Convert mentions to JSON
	mentionsJSON := ""
	if len(req.Mentions) > 0 {
		b, _ := json.Marshal(req.Mentions)
		mentionsJSON = string(b)
	}

	msg := &store.Message{
		ID:               msgID,
		Sender:           "user",
		Recipient:        "conv:" + convID,
		Kind:             "chat",
		Payload:          req.Payload,
		CreatedAt:        now,
		ConvID:           convID,
		Mentions:         mentionsJSON,
		RequiresApproval: false,
		ApprovalState:    "n/a",
	}

	seq, err := s.st.InsertMessage(msg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "failed to insert message")
		return
	}

	// Reset agent streak (user message resets it)
	_ = s.st.UpdateConversationStreak(convID, 0)

	// Audit log
	_ = s.st.AppendAudit("admin", "message.sent_as_user",
		fmt.Sprintf("conv_id=%s seq=%d", convID, seq), now)

	writeJSON(w, map[string]any{
		"ok":  true,
		"seq": seq,
	})
}

// extractMentions extracts @peer-id patterns from text.
// Pattern: @[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}
var mentionPattern = regexp.MustCompile(`@([a-zA-Z0-9][a-zA-Z0-9_-]{0,63})`)

func extractMentions(text string) []string {
	matches := mentionPattern.FindAllStringSubmatch(text, -1)
	seen := make(map[string]bool)
	var result []string
	for _, m := range matches {
		if len(m) > 1 && !seen[m[1]] {
			seen[m[1]] = true
			result = append(result, m[1])
		}
	}
	return result
}

// handleAdminGetConversationMessages handles GET /admin/conversations/{id}/messages.
// Get all messages in a conversation (admin view, no member check).
func (s *Server) handleAdminGetConversationMessages(w http.ResponseWriter, r *http.Request) {
	convID := strings.TrimPrefix(r.URL.Path, "/admin/conversations/")
	convID = strings.TrimSuffix(convID, "/messages")

	// Parse limit
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
	}

	// Get conversation messages
	// For now, use a simple query (TODO: add specific ConversationMessages method)
	allMessages, err := s.st.ThreadMessages(convID, 1000)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}

	// Filter to conversation messages
	var messages []*store.Message
	for _, m := range allMessages {
		if m.ConvID == convID {
			messages = append(messages, m)
		}
	}

	// Get latest seq
	latestSeq, _ := s.st.ConversationMaxSeq(convID)

	writeJSON(w, map[string]any{
		"ok":         true,
		"messages":   messages,
		"latest_seq": latestSeq,
	})
}

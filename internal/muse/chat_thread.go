package muse

// chat_thread.go serves the console's conversation view from the in-memory
// job map. It deliberately does not persist anything and never reads Muse's
// chat.history: only jobs this process admitted in the last hour are visible.
// Operator text and reply text appear here (admin-only) and nowhere else; the
// per-job GET snapshot remains text-free.

import (
	"sort"
	"strings"
	"time"
)

// ChatThreadJob is one conversation turn as shown on the direct-chat page.
// Text is the operator's already-sent message; reply text stays in Reply.
type ChatThreadJob struct {
	ChatID       string     `json:"chat_id"`
	Status       string     `json:"status"`
	Session      string     `json:"session"`
	Visibility   string     `json:"visibility"`
	Text         string     `json:"text"`
	Reply        *ChatReply `json:"reply,omitempty"`
	PartialText  string     `json:"partial_text,omitempty"`
	PartialDraft bool       `json:"draft,omitempty"`
	Warnings     []string   `json:"warnings"`
	Confidence   string     `json:"confidence"`
	Error        *ChatError `json:"error,omitempty"`
	Cancel       string     `json:"cancel,omitempty"`
	BudgetSecs   int        `json:"budget_secs"`
	UserSeq      int64      `json:"user_seq"`
	SentAt       int64      `json:"sent_at"`
	ConfirmedAt  int64      `json:"confirmed_at"`
	DeadlineAt   int64      `json:"deadline_at"`
	CreatedAt    int64      `json:"created_at"`
	UpdatedAt    int64      `json:"updated_at"`
}

// ChatThreadView is the page payload. retention_secs is the only history
// boundary the server promises; the console renders the boundary hint itself.
type ChatThreadView struct {
	PeerID        string          `json:"peer_id"`
	Session       string          `json:"session"`
	RetentionSecs int             `json:"retention_secs"`
	Since         int64           `json:"since"`
	ServerTime    int64           `json:"server_time"`
	Jobs          []ChatThreadJob `json:"jobs"`
	// Sessions lists the side-chat session ids seen for this peer in the
	// window, so the header selector can offer real threads only.
	Sessions []string `json:"sessions"`
}

// ChatThread returns one peer's in-memory conversation for the selected
// session ("" = member main chat). Unknown peers are a 404; an invalid session
// id is a 400. No DB read, no chat.history call.
func (s *Service) ChatThread(peerID, sessionID string) (*ChatThreadView, error) {
	if s == nil {
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}
	peerID = strings.TrimSpace(peerID)
	if peerID == "" {
		return nil, admissionErr(400, "unknown_member", 0)
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID != "" && !chatSessionIDPattern.MatchString(sessionID) {
		return nil, admissionErr(400, "invalid_session_id", 0)
	}
	peer, err := s.st.GetPeer(peerID)
	if err != nil {
		return nil, err
	}
	if peer == nil {
		return nil, admissionErr(404, "unknown_member", 0)
	}

	now := s.now().Unix()
	since := now - int64(chatJobRetention/time.Second)
	s.chatMu.Lock()
	s.gcChatLocked(now)
	jobs := make([]*ChatJob, 0, 8)
	for _, job := range s.chatJobs {
		if job.PeerID != peerID || job.CreatedAt < since {
			continue
		}
		jobs = append(jobs, job)
	}
	s.chatMu.Unlock()

	out := &ChatThreadView{
		PeerID: peerID, Session: sessionID,
		RetentionSecs: int(chatJobRetention / time.Second),
		Since:         since, ServerTime: now,
		Jobs: []ChatThreadJob{}, Sessions: []string{},
	}
	seen := map[string]bool{}
	for _, job := range jobs {
		view := job.threadView()
		if view.Session != "" && !seen[view.Session] {
			seen[view.Session] = true
			out.Sessions = append(out.Sessions, view.Session)
		}
		if job.SessionID != sessionID {
			continue
		}
		out.Jobs = append(out.Jobs, view)
	}
	sort.Strings(out.Sessions)
	sort.SliceStable(out.Jobs, func(i, j int) bool {
		if out.Jobs[i].CreatedAt == out.Jobs[j].CreatedAt {
			return out.Jobs[i].ChatID < out.Jobs[j].ChatID
		}
		return out.Jobs[i].CreatedAt < out.Jobs[j].CreatedAt
	})
	return out, nil
}

func (j *ChatJob) threadView() ChatThreadJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	v := ChatThreadJob{
		ChatID: j.ID, Status: j.Status, Session: j.SessionID, Visibility: j.Visibility,
		Text: j.Text, Reply: j.Reply, PartialText: j.Partial, PartialDraft: j.Draft,
		Warnings: append([]string(nil), j.Warnings...), Confidence: j.Conf,
		Error: j.Err, Cancel: j.Cancel, BudgetSecs: int(j.Budget / time.Second),
		UserSeq: j.UserSeq, SentAt: j.SentAt, ConfirmedAt: j.Confirm,
		DeadlineAt: j.Deadline, CreatedAt: j.CreatedAt, UpdatedAt: j.Updated,
	}
	if v.Warnings == nil {
		v.Warnings = []string{}
	}
	if v.Confidence == "" {
		v.Confidence = "unknown"
	}
	return v
}

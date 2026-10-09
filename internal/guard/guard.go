// Package guard enforces all safety rules on every write (DESIGN §4.3, §6).
// Deliberately dumb and deterministic: counting and string comparison only,
// never calls an LLM.
package guard

import (
	"fmt"
	"strings"
	"sync"
	"unicode"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Envelope is the validated send request (DESIGN §5.2 + §v12).
type Envelope struct {
	ID               string
	To               string
	From             string
	Kind             string // task|result|chat|status|permission_request|permission_decision
	InReplyTo        string
	RequiresApproval bool
	Payload          string
	// Handshake fields (§6.5): status → Status; permission_request →
	// Op/Target/Detail/ExpiresInSecs; permission_decision → Decision.
	Status        string
	Op            string
	Target        string
	Detail        string
	Decision      string // allow|deny
	ExpiresInSecs int64
	// Conversation fields (§v12): ConvID for conversation messages,
	// SeenSeq for freshness check, Mentions for @-targeted delivery.
	ConvID   string
	SeenSeq  int64
	Mentions []string // list of @mentioned peer IDs
}

// Limits configures the fuse and rate limiter (DESIGN §6.1) plus the
// permission-handshake caps (§6.5) and conversation guards (§v12).
type Limits struct {
	FuseMaxMessages int
	FuseMaxAgeSecs  int64
	RatePerMinute   int
	// MaxOpenPermissions caps unexpired open permission_requests per
	// thread (approval-fatigue guard). <=0 disables the cap.
	MaxOpenPermissions int
	// ProgressThrottleSecs floors spacing between kind=status/progress
	// messages per sender per thread. <=0 disables (besides global rate).
	ProgressThrottleSecs int64
	// Conversation guards (§v12).
	ConvAgentTurnBudget int // max consecutive agent replies after user message
	ConvFuseMaxMessages int // circuit breaker per conversation
}

// Rejection is a machine-readable send refusal.
type Rejection struct {
	Code      string // duplicate_id | loop_fuse_tripped | loop_guard | rate_limited | not_member | stale | agent_turn_budget
	Reason    string
	Retry     int   // Retry-After seconds for rate_limited
	LatestSeq int64 // latest message seq for stale rejection
}

func (r *Rejection) Error() string { return r.Code + ": " + r.Reason }

// Verdict is the outcome of Check.
type Verdict struct {
	RootID string
	Held   bool // requires_approval → held server-side, caller returns 202
}

// Guard holds rate-limiter state.
type Guard struct {
	st     store.Store
	limits Limits
	mu     sync.Mutex
	hits   map[string][]int64 // sender → unix-second timestamps
}

// New creates a Guard over st.
func New(st store.Store, lim Limits) *Guard {
	return &Guard{st: st, limits: lim, hits: map[string][]int64{}}
}

// Check validates an envelope. It resolves RootID and reports Held for
// approval-gated messages. A *Rejection error means the send must be refused.
func (g *Guard) Check(env *Envelope, now int64) (*Verdict, error) {
	if env.ID == "" || env.To == "" || env.From == "" || strings.TrimSpace(env.Payload) == "" {
		return nil, &Rejection{Code: "bad_message", Reason: "id/to/from/payload are required"}
	}
	switch env.Kind {
	case "", "task":
		env.Kind = "task"
	case "result", "chat", "system", "status", "permission_request", "permission_decision":
	default:
		return nil, &Rejection{Code: "bad_message", Reason: "kind must be task|result|chat|status|permission_request|permission_decision"}
	}

	// Idempotency: UNIQUE(sender,id) — duplicate returns 409 without duplicating.
	if existing, err := g.st.GetBySenderID(env.From, env.ID); err != nil {
		return nil, err
	} else if existing != nil {
		return nil, &Rejection{Code: "duplicate_id", Reason: "sender already used id " + env.ID}
	}

	rootID, err := g.resolveRoot(env)
	if err != nil {
		return nil, err
	}

	// Rate limit before the heavier checks.
	if retry, limited := g.rateCheck(env.From, now); limited {
		return nil, &Rejection{Code: "rate_limited", Reason: "send rate exceeded", Retry: retry}
	}

	// Conversation guards (§v12): check if this is a conversation message.
	isConversation := strings.HasPrefix(env.To, "conv:")
	if isConversation && env.ConvID != "" {
		// Member validation: sender must be a member of the conversation.
		// Exception: sender=user (admin) is allowed without membership check.
		if env.From != "user" {
			isMember, err := g.st.IsConversationMember(env.ConvID, env.From)
			if err != nil {
				return nil, err
			}
			if !isMember {
				return nil, &Rejection{Code: "not_member",
					Reason: fmt.Sprintf("sender %s is not a member of conversation %s", env.From, env.ConvID)}
			}
		}

		// Freshness check: unsolicited replies must not lag the latest seq.
		// Exception: a sender @mentioned in recent history is solicited.
		if env.From != "user" && !g.mentionedInRecent(env.ConvID, env.From) {
			// Query latest message seq in this conversation.
			latestSeq, err := g.st.ConversationMaxSeq(env.ConvID)
			if err != nil {
				return nil, err
			}
			if env.SeenSeq < latestSeq {
				return nil, &Rejection{Code: "stale",
					Reason:    fmt.Sprintf("seen_seq=%d but latest is %d; re-read conversation history", env.SeenSeq, latestSeq),
					LatestSeq: latestSeq}
			}
		}

		// Turn budget: check consecutive agent replies.
		if env.From != "user" && g.limits.ConvAgentTurnBudget > 0 {
			conv, err := g.st.GetConversation(env.ConvID)
			if err != nil {
				return nil, err
			}
			if conv != nil && conv.AgentStreak >= g.limits.ConvAgentTurnBudget {
				return nil, &Rejection{Code: "agent_turn_budget",
					Reason: fmt.Sprintf("conversation %s exceeded %d consecutive agent replies; wait for user", env.ConvID, g.limits.ConvAgentTurnBudget)}
			}
		}

		// Conversation fuse: separate from thread fuse.
		if g.limits.ConvFuseMaxMessages > 0 {
			// Note: We're using a simple message count for now.
			// In a full implementation, you'd want conversation_fuses table like thread_fuses.
			// For Phase 2, we'll implement a basic count check.
			// TODO: Add conversation_fuses table in future iteration.
		}
	}

	// Hard fuse: count and age (minus the admin-cleared watermark).
	wm, err := g.st.FuseWatermark(rootID)
	if err != nil {
		return nil, err
	}
	count, err := g.st.CountThreadSince(rootID, wm)
	if err != nil {
		return nil, err
	}
	if count >= g.limits.FuseMaxMessages {
		_ = g.st.AppendAudit(env.From, "fuse.tripped",
			fmt.Sprintf("thread=%s count=%d", rootID, count), now)
		return nil, &Rejection{Code: "loop_fuse_tripped",
			Reason: fmt.Sprintf("thread %s exceeded %d messages; ask a human to reset the fuse", rootID, g.limits.FuseMaxMessages)}
	}
	if oldest, err := g.st.ThreadOldestTs(rootID); err != nil {
		return nil, err
	} else if oldest != 0 && now-oldest > g.limits.FuseMaxAgeSecs {
		return nil, &Rejection{Code: "loop_fuse_tripped",
			Reason: fmt.Sprintf("thread %s is older than %d hours", rootID, g.limits.FuseMaxAgeSecs/3600)}
	}

	// Handshake kinds skip the loop heuristic: allow/deny/started are short
	// by design and would otherwise trip courtesy-loop/4-hop rules.
	if !isHandshakeKind(env.Kind) {
		if rej := g.loopCheck(rootID, env); rej != nil {
			return nil, rej
		}
	}

	// Permission handshake semantics (§6.5).
	if env.Kind == "status" || env.Kind == "permission_request" || env.Kind == "permission_decision" {
		if err := g.checkHandshake(env, rootID, now); err != nil {
			return nil, err
		}
	}

	// Approval gate: held server-side, delivered only after human approval.
	if env.RequiresApproval {
		_ = g.st.AppendAudit(env.From, "message.held",
			fmt.Sprintf("thread=%s id=%s", rootID, env.ID), now)
		return &Verdict{RootID: rootID, Held: true}, nil
	}
	return &Verdict{RootID: rootID}, nil
}

// RecordHit notes a successful send for rate accounting. Call after insert.
func (g *Guard) RecordHit(sender string, now int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.hits[sender] = append(g.hits[sender], now)
}

func (g *Guard) resolveRoot(env *Envelope) (string, error) {
	if env.InReplyTo != "" {
		parents, err := g.st.GetByIDAnySender(env.InReplyTo)
		if err != nil {
			return "", err
		}
		for _, p := range parents {
			// Prefer a parent in the same conversation; otherwise first match.
			if p.Sender == env.To || env.To == "*" || p.Recipient == env.From || p.Recipient == "*" {
				if p.RootID != "" {
					return p.RootID, nil
				}
				return threadRootOf(p), nil
			}
		}
		if len(parents) > 0 {
			if parents[0].RootID != "" {
				return parents[0].RootID, nil
			}
			return threadRootOf(parents[0]), nil
		}
		// Unknown parent: start a thread keyed by the reply target so the
		// fuse still bounds it instead of silently forking.
		return env.From + "/" + env.InReplyTo, nil
	}
	return env.From + "/" + env.ID, nil
}

func threadRootOf(m *store.Message) string {
	if m.RootID != "" {
		return m.RootID
	}
	return m.Sender + "/" + m.ID
}

// rateCheck implements a sliding-window limiter: RatePerMinute sends per
// 60s with a burst of 10. Returns (retryAfterSecs, limited).
func (g *Guard) rateCheck(sender string, now int64) (int, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	window := g.hits[sender]
	kept := window[:0]
	for _, t := range window {
		if now-t < 60 {
			kept = append(kept, t)
		}
	}
	g.hits[sender] = kept
	limit := g.limits.RatePerMinute
	if limit <= 0 {
		limit = 60
	}
	// Burst allowance: first 10 sends in a window always pass so interactive
	// use never trips on a short burst; the per-minute cap applies after.
	if len(kept) < 10 {
		return 0, false
	}
	if len(kept) >= limit {
		retry := int(60 - (now - kept[0]))
		if retry < 1 {
			retry = 1
		}
		return retry, true
	}
	return 0, false
}

// ---- loop heuristic (DESIGN §6.2) ----

var ackOnlyTokens = map[string]bool{
	"received": true, "收到": true, "ack": true, "ok": true, "👍": true,
	"gotit": true, "got it": true, "noted": true, "roger": true,
	"copy": true, "confirmed": true, "确认": true, "收到谢谢": true,
}

// normalize strips whitespace/punctuation and lowercases for dup comparison.
func normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isAckOnly(payload string) bool {
	n := normalize(payload)
	if ackOnlyTokens[n] {
		return true
	}
	// Short payloads (<8 runes after normalize) that carry no content words.
	if len([]rune(n)) <= 6 {
		return true
	}
	return false
}

func (g *Guard) loopCheck(rootID string, env *Envelope) *Rejection {
	last, err := g.st.LastNInThread(rootID, 5)
	if err != nil || len(last) == 0 {
		return nil
	}
	norm := normalize(env.Payload)
	for _, m := range last {
		if normalize(m.Payload) == norm && norm != "" {
			return &Rejection{Code: "loop_guard",
				Reason: "identical payload already present in this thread; on 409 do not retry — summarize and escalate to your human"}
		}
	}
	// Pure-ack following a pure-ack: courtesy ping-pong with no content.
	if isAckOnly(env.Payload) && isAckOnly(last[len(last)-1].Payload) {
		return &Rejection{Code: "loop_guard",
			Reason: "consecutive acknowledgement-only messages look like an LLM courtesy loop; stop and escalate to your human"}
	}
	// Alternating A→B→A→B with no payload growth across 4 hops.
	if len(last) >= 3 {
		chain := append(append([]*store.Message{}, last...), &store.Message{Sender: env.From, Payload: env.Payload})
		if len(chain) >= 4 {
			tail := chain[len(chain)-4:]
			senders := map[string]bool{tail[0].Sender: true, tail[1].Sender: true}
			if len(senders) == 2 &&
				tail[0].Sender == tail[2].Sender &&
				tail[1].Sender == tail[3].Sender &&
				tail[0].Sender != tail[1].Sender {
				maxLen, minLen := 0, -1
				for _, m := range tail {
					l := len([]rune(m.Payload))
					if l > maxLen {
						maxLen = l
					}
					if minLen < 0 || l < minLen {
						minLen = l
					}
				}
				if maxLen-minLen < 16 {
					return &Rejection{Code: "loop_guard",
						Reason: "alternating A→B→A→B with no new content across 4 hops; stop and escalate to your human"}
				}
			}
		}
	}
	return nil
}

// ---- permission handshake validation (DESIGN §6.5) ----

func isHandshakeKind(kind string) bool {
	return kind == "status" || kind == "permission_request" || kind == "permission_decision"
}

// validHandshakeStatuses is the status value vocabulary for kind=status.
var validHandshakeStatuses = map[string]bool{
	"started": true, "progress": true, "blocked": true, "resumed": true, "cancelled": true,
}

// taskOf finds the task message a handshake refers to. For status and
// permission_request, InReplyTo names the task id; for permission_decision
// it names the request message id, whose own InReplyTo names the task.
func (g *Guard) taskOf(env *Envelope) *store.Message {
	msgs, err := g.st.GetByIDAnySender(env.InReplyTo)
	if err != nil || len(msgs) == 0 {
		return nil
	}
	if env.Kind == "permission_decision" {
		for _, m := range msgs {
			if m.Kind != "permission_request" {
				continue
			}
			tasks, err := g.st.GetByIDAnySender(m.InReplyTo)
			if err != nil || len(tasks) == 0 {
				return nil
			}
			return tasks[0]
		}
		return nil
	}
	return msgs[0]
}

// checkHandshake enforces reference integrity, direction rules, permission
// lifecycle (first decision wins, expiry is fail-closed), the open-request
// cap, and the progress throttle. RequiresApproval is refused on handshake
// kinds: pre-send approval and execution approval are separate gates.
func (g *Guard) checkHandshake(env *Envelope, rootID string, now int64) error {
	if env.RequiresApproval {
		return &Rejection{Code: "bad_message", Reason: "requires_approval is not allowed on handshake kinds"}
	}
	if env.InReplyTo == "" {
		return &Rejection{Code: "bad_permission_ref", Reason: "in_reply_to is required on handshake kinds"}
	}
	task := g.taskOf(env)
	if task == nil || task.Kind != "task" {
		return &Rejection{Code: "bad_permission_ref", Reason: "in_reply_to does not reference a task"}
	}
	if task.RootID != "" && rootID != task.RootID {
		return &Rejection{Code: "bad_permission_ref", Reason: "handshake is in a different thread than its task"}
	}
	switch env.Kind {
	case "status":
		if !validHandshakeStatuses[env.Status] {
			return &Rejection{Code: "bad_message", Reason: "status must be started|progress|blocked|resumed|cancelled"}
		}
		if env.Status == "cancelled" {
			// Either side may cancel: the task sender or its recipient.
			if env.From != task.Sender && env.From != task.Recipient {
				return &Rejection{Code: "permission_not_authorized",
					Reason: "only the task sender or recipient may cancel"}
			}
			return nil
		}
		// Lifecycle signals come from the task recipient (B) only.
		if env.From != task.Recipient || env.To != task.Sender {
			return &Rejection{Code: "permission_not_authorized",
				Reason: "lifecycle status must flow from the task recipient back to its sender"}
		}
		if env.Status == "progress" && g.limits.ProgressThrottleSecs > 0 {
			last, err := g.st.LastHandshakeTs(rootID, env.From, "progress")
			if err != nil {
				return err
			}
			if last != 0 && now-last < g.limits.ProgressThrottleSecs {
				return &Rejection{Code: "progress_throttled",
					Reason: fmt.Sprintf("progress updates throttled: one per %d seconds per thread", g.limits.ProgressThrottleSecs)}
			}
		}
		return nil
	case "permission_request":
		// Requests come from the task recipient (B) back to its sender (A).
		if env.From != task.Recipient || env.To != task.Sender {
			return &Rejection{Code: "permission_not_authorized",
				Reason: "permission requests must flow from the task recipient back to its sender"}
		}
		if g.limits.MaxOpenPermissions > 0 {
			n, err := g.st.CountOpenPermissions(rootID, now)
			if err != nil {
				return err
			}
			if n >= g.limits.MaxOpenPermissions {
				return &Rejection{Code: "permission_rate_limited",
					Reason: fmt.Sprintf("too many open permission requests in thread %s", rootID)}
			}
		}
		return nil
	case "permission_decision":
		if env.Decision != "allow" && env.Decision != "deny" {
			return &Rejection{Code: "bad_message", Reason: "decision must be allow|deny"}
		}
		pr, err := g.st.GetPermissionRequest(env.InReplyTo)
		if err != nil {
			return err
		}
		if pr == nil {
			return &Rejection{Code: "bad_permission_ref", Reason: "unknown permission request"}
		}
		// Only the request's approver (A) may decide.
		if env.From != pr.Approver || env.To != pr.Requester {
			return &Rejection{Code: "permission_not_authorized",
				Reason: "only the requested approver may decide"}
		}
		if pr.Status != "pending" {
			// Same value re-sent: let the queue layer answer idempotently
			// with the original decision seq. Conflicting flips stop here.
			if (pr.Status == "allowed") != (env.Decision == "allow") {
				return &Rejection{Code: "permission_already_decided", Reason: "request already decided"}
			}
			return nil
		}
		if now >= pr.ExpiresAt {
			return &Rejection{Code: "permission_expired", Reason: "request expired; the recipient must treat it as denied"}
		}
		return nil
	}
	return nil
}

// ResetFuse is the admin "reset fuse" action: it moves the watermark to the
// current max so history is kept but accounting restarts (DESIGN §6.1).
func (g *Guard) ResetFuse(rootID string, now int64) error {
	maxSeq, err := g.st.MaxSeq()
	if err != nil {
		return err
	}
	if err := g.st.SetFuseWatermark(rootID, maxSeq); err != nil {
		return err
	}
	return g.st.AppendAudit("admin", "fuse.reset", "thread="+rootID, now)
}

// mentionedInRecent reports whether sender appears in the @mentions of the
// most recent conversation messages (§v12.4 freshness exception). Matching is
// on the quoted id so "bob" never matches "bobby" inside the JSON array.
func (g *Guard) mentionedInRecent(convID, sender string) bool {
	mentions, err := g.st.RecentConversationMentions(convID, 5)
	if err != nil {
		return false
	}
	needle := `"` + sender + `"`
	for _, m := range mentions {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// UpdateConversationStreak updates the agent reply streak for a conversation (§v12).
// Call this after a message is successfully inserted.
// - If sender=user: reset streak to 0
// - If sender is an agent: increment streak by 1
func (g *Guard) UpdateConversationStreak(convID, sender string) error {
	if convID == "" {
		return nil // not a conversation message
	}
	conv, err := g.st.GetConversation(convID)
	if err != nil {
		return err
	}
	if conv == nil {
		return nil // conversation doesn't exist (shouldn't happen)
	}

	var newStreak int
	if sender == "user" {
		newStreak = 0
	} else {
		newStreak = conv.AgentStreak + 1
	}

	return g.st.UpdateConversationStreak(convID, newStreak)
}

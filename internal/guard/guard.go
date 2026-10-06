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

// Envelope is the validated send request (DESIGN §5.2).
type Envelope struct {
	ID               string
	To               string
	From             string
	Kind             string // task|result|chat
	InReplyTo        string
	RequiresApproval bool
	Payload          string
}

// Limits configures the fuse and rate limiter (DESIGN §6.1).
type Limits struct {
	FuseMaxMessages int
	FuseMaxAgeSecs  int64
	RatePerMinute   int
}

// Rejection is a machine-readable send refusal.
type Rejection struct {
	Code   string // duplicate_id | loop_fuse_tripped | loop_guard | rate_limited
	Reason string
	Retry  int // Retry-After seconds for rate_limited
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
	case "result", "chat", "system":
	default:
		return nil, &Rejection{Code: "bad_message", Reason: "kind must be task|result|chat"}
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

	// Loop heuristic over the last 5 messages in the thread.
	if rej := g.loopCheck(rootID, env); rej != nil {
		return nil, rej
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

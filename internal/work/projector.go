package work

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/stages"
	"github.com/AlixWang/agent-relay/internal/store"
)

// TicketView is a store.Ticket plus the presence-dependent fields the console
// needs. EffectiveStage is not persisted because presence and lease deadlines
// change at any time.
type TicketView struct {
	store.Ticket
	EffectiveStage string                      `json:"effective_stage"`
	Attention      bool                        `json:"attention"`
	AssigneeOnline bool                        `json:"assignee_online"`
	LeaseExpired   bool                        `json:"lease_expired,omitempty"`
	Assignment     *store.TicketAssignment     `json:"assignment,omitempty"`
	Assignments    []*store.TicketAssignment   `json:"assignments,omitempty"`
	Subscriptions  []*store.TicketSubscription `json:"subscriptions,omitempty"`
	Mandate        *store.Mandate              `json:"mandate,omitempty"`
}

// Service owns the message → ticket projection loop.
type Service struct {
	st            store.Store
	onlineSecs    int64
	policyVersion string
	now           func() int64

	mu          sync.RWMutex
	ready       bool
	lastErr     string
	cursor      int64
	lastRefresh int64
}

// New creates a projection service. onlineSecs is the presence window used
// only for the person/offline display overlay.
func New(st store.Store, onlineSecs int64) *Service {
	return &Service{
		st:         st,
		onlineSecs: onlineSecs,
		now:        func() int64 { return time.Now().Unix() },
	}
}

// SetPolicyVersion records the deterministic admission policy version on
// newly projected tickets. It must be called before Start/Rebuild.
func (s *Service) SetPolicyVersion(v string) { s.policyVersion = v }

// Start rebuilds the projection in the background and then runs a 2 second
// pass loop. It never blocks the relay's HTTP server; projection is a read
// model and must not affect message delivery.
func (s *Service) Start(ctx context.Context) {
	go func() {
		for {
			if err := s.Rebuild(ctx); err != nil {
				s.setErr(err)
				select {
				case <-ctx.Done():
					return
				case <-time.After(60 * time.Second):
					continue
				}
			}
			break
		}
		s.loop(ctx)
	}()
}

// Rebuild clears and replays the whole message log. Safe to call repeatedly.
func (s *Service) Rebuild(ctx context.Context) error {
	s.mu.Lock()
	s.ready = false
	s.lastErr = ""
	s.mu.Unlock()
	if err := s.st.ResetTickets(); err != nil {
		return fmt.Errorf("reset tickets: %w", err)
	}
	start := time.Now()
	var after int64
	var messages int
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := s.st.MessagesAfter(after, 500)
		if err != nil {
			return fmt.Errorf("scan messages after %d: %w", after, err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if err := s.project(m); err != nil {
				return fmt.Errorf("project seq %d: %w", m.Seq, err)
			}
			messages++
			after = m.Seq
		}
	}
	maxSeq, err := s.st.MaxSeq()
	if err != nil {
		return fmt.Errorf("max seq: %w", err)
	}
	_, _ = s.st.PruneOrphanTickets()
	if _, err := s.st.ExpireAssignmentLeases(s.now()); err != nil {
		return fmt.Errorf("expire assignment leases: %w", err)
	}
	tickets, mappings, _ := s.st.TicketStats()
	s.mu.Lock()
	s.cursor = after
	s.ready = true
	s.lastErr = ""
	s.lastRefresh = s.now()
	s.mu.Unlock()
	log.Printf("work: rebuilt tickets=%d mappings=%d messages=%d cursor=%d duration=%s",
		tickets, mappings, messages, maxSeq, time.Since(start).Round(time.Millisecond))
	return nil
}

func (s *Service) loop(ctx context.Context) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Pass(ctx); err != nil {
				s.setErr(err)
				log.Printf("work: pass: %v", err)
			}
		}
	}
}

// Pass projects new messages and periodically refreshes open tickets for
// events that do not insert a message (approval, permission expiry).
func (s *Service) Pass(ctx context.Context) error {
	s.mu.RLock()
	cursor := s.cursor
	lastRefresh := s.lastRefresh
	s.mu.RUnlock()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		msgs, err := s.st.MessagesAfter(cursor, 500)
		if err != nil {
			return fmt.Errorf("scan after %d: %w", cursor, err)
		}
		if len(msgs) == 0 {
			break
		}
		for _, m := range msgs {
			if err := s.project(m); err != nil {
				return fmt.Errorf("project seq %d: %w", m.Seq, err)
			}
			cursor = m.Seq
			s.mu.Lock()
			s.cursor = cursor
			s.mu.Unlock()
		}
	}
	if _, err := s.st.ExpireAssignmentLeases(s.now()); err != nil {
		return fmt.Errorf("expire assignment leases: %w", err)
	}
	if s.now()-lastRefresh >= 30 {
		if err := s.RefreshOpen(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.lastRefresh = s.now()
		s.mu.Unlock()
	}
	s.mu.Lock()
	s.ready = true
	s.lastErr = ""
	s.mu.Unlock()
	return nil
}

// RefreshOpen recomputes every open ticket. This catches approval_state
// changes and expired permission requests that never created a message.
func (s *Service) RefreshOpen(ctx context.Context) error {
	ids, err := s.st.OpenTicketIDs()
	if err != nil {
		return fmt.Errorf("open ticket ids: %w", err)
	}
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := s.refresh(id); err != nil {
			return fmt.Errorf("refresh ticket %d: %w", id, err)
		}
	}
	return nil
}

func (s *Service) project(m *store.Message) error {
	if id, err := s.st.MessageTicket(m.Seq); err != nil {
		return err
	} else if id != 0 {
		return s.refresh(id)
	}
	if isEligibleTask(m) {
		if err := s.ensureTicket(m); err != nil {
			return err
		}
		return s.refresh(m.Seq)
	}
	if m.InReplyTo == "" {
		return nil
	}
	id, err := s.resolveParentTicket(m)
	if err != nil {
		return err
	}
	if id == 0 {
		return nil
	}
	if err := s.st.SetMessageTicket(m.Seq, id); err != nil {
		return err
	}
	return s.refresh(id)
}

func isEligibleTask(m *store.Message) bool {
	if m == nil || m.Kind != "task" || m.Sender == "system" {
		return false
	}
	if strings.HasPrefix(m.RootID, "verify/") {
		return false
	}
	switch {
	case m.Recipient == "*", m.Recipient == "operator", m.Recipient == "system":
		return false
	case strings.HasPrefix(m.Recipient, "grp_"):
		return false
	}
	return m.Recipient != ""
}

func (s *Service) ensureTicket(task *store.Message) error {
	if t, err := s.st.GetTicket(task.Seq); err != nil {
		return err
	} else if t != nil {
		return s.st.SetMessageTicket(task.Seq, task.Seq)
	}
	root := task.RootID
	if root == "" {
		root = task.Sender + "/" + task.ID
	}
	t := &store.Ticket{
		ID:         task.Seq,
		ThreadRoot: root,
		Requester:  task.Sender,
		Assignee:   task.Recipient,
		State:      "open",
		Stage:      "queued",
		CreatedAt:  task.CreatedAt,
		UpdatedAt:  task.CreatedAt,
		LastSeq:    task.Seq,
	}
	if err := s.st.CreateTicket(t); err != nil {
		return err
	}
	if err := s.st.SetMessageTicket(task.Seq, task.Seq); err != nil {
		return err
	}
	if _, err := s.st.EnsureAssignment(store.AssignmentCommand{
		TicketID: task.Seq, Assignee: task.Recipient, Now: s.now(),
	}); err != nil {
		return err
	}
	if err := s.st.CreateMandate(&store.Mandate{
		TicketID: task.Seq, PolicyVersion: s.policyVersion, Scope: "task",
		Requester: task.Sender, RequesterKind: requesterKind(task.Sender), CreatedAt: task.CreatedAt,
	}); err != nil {
		return err
	}
	return s.st.SubscribeTicket(task.Seq, root, "own", s.now())
}

func (s *Service) resolveParentTicket(m *store.Message) (int64, error) {
	parentID := m.InReplyTo
	seenIDs := map[string]bool{}
	seenSeqs := map[int64]bool{}
	for depth := 0; depth < 64 && parentID != ""; depth++ {
		if seenIDs[parentID] {
			return 0, nil
		}
		seenIDs[parentID] = true
		cands, err := s.st.GetByIDAnySender(parentID)
		if err != nil {
			return 0, err
		}
		parent := pickParent(cands, m.RootID)
		if parent == nil || seenSeqs[parent.Seq] {
			return 0, nil
		}
		seenSeqs[parent.Seq] = true
		if id, err := s.st.MessageTicket(parent.Seq); err != nil {
			return 0, err
		} else if id != 0 {
			return id, nil
		}
		if isEligibleTask(parent) {
			if err := s.ensureTicket(parent); err != nil {
				return 0, err
			}
			return parent.Seq, nil
		}
		parentID = parent.InReplyTo
	}
	return 0, nil
}

func pickParent(cands []*store.Message, root string) *store.Message {
	if len(cands) == 0 {
		return nil
	}
	for _, m := range cands {
		if root != "" && m.RootID == root {
			return m
		}
	}
	return cands[0]
}

func (s *Service) refresh(id int64) error {
	task, err := s.st.GetBySeq(id)
	if err != nil || task == nil {
		return err
	}
	t, err := s.st.GetTicket(id)
	if err != nil {
		return err
	}
	if t == nil {
		return nil
	}
	now := s.now()
	msgs, err := s.st.TicketMessages(id, 2000)
	if err != nil {
		return err
	}
	hasOpen, err := s.st.TicketHasOpenPermission(id, now)
	if err != nil {
		return err
	}
	d := Derive(task, msgs, hasOpen)
	t.ThreadRoot = task.RootID
	if t.ThreadRoot == "" {
		t.ThreadRoot = task.Sender + "/" + task.ID
	}
	t.Requester = task.Sender
	if t.Assignee == "" {
		t.Assignee = task.Recipient
	}
	t.State = d.State
	t.Stage = d.Stage
	t.UpdatedAt = d.UpdatedAt
	t.EndedAt = d.EndedAt
	t.LastSeq = d.LastSeq
	if err := s.st.SubscribeTicket(id, t.ThreadRoot, "own", now); err != nil {
		return err
	}
	if m, err := s.st.GetMandate(id); err != nil {
		return err
	} else if m == nil {
		if err := s.st.CreateMandate(&store.Mandate{
			TicketID: id, PolicyVersion: s.policyVersion, Scope: "task",
			Requester: task.Sender, RequesterKind: requesterKind(task.Sender), CreatedAt: task.CreatedAt,
		}); err != nil {
			return err
		}
	}
	latest, err := s.st.LatestAssignment(id)
	if err != nil {
		return err
	}
	leaseExpired := latest != nil && latest.Status == store.AssignmentEnded &&
		latest.EndedReason == store.AssignmentReasonLease
	if d.State == "open" {
		// An expired lease is not silently re-armed: the operator must
		// reassign, and until then the ticket shows as person/offline.
		if !leaseExpired {
			if _, err := s.st.EnsureAssignment(store.AssignmentCommand{
				TicketID: id, Assignee: t.Assignee, Now: now,
			}); err != nil {
				return err
			}
			if d.Stage != stages.Queued && d.Stage != stages.PersonApproval {
				if err := s.st.MarkAssignmentRunning(id, now); err != nil {
					return err
				}
			}
		}
	} else {
		if err := s.st.EndAssignments(id, store.AssignmentReasonClosed, now); err != nil {
			return err
		}
	}
	return s.st.UpdateTicket(t)
}

// List returns a page of tickets with effective stages. Attention filtering
// includes the offline overlay, which is why it is applied after decoration.
func (s *Service) List(f store.TicketFilter, limit, offset int) ([]*TicketView, int, error) {
	if f.Stage == "attention" {
		f.Stage = ""
		f.State = "open"
		items, _, err := s.st.ListTickets(f, 200, 0)
		if err != nil {
			return nil, 0, err
		}
		views := s.decorate(items)
		filtered := make([]*TicketView, 0, len(views))
		for _, v := range views {
			if v.Attention {
				filtered = append(filtered, v)
			}
		}
		sortViews(filtered)
		total := len(filtered)
		return pageViews(filtered, limit, offset), total, nil
	}
	items, total, err := s.st.ListTickets(f, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	return s.decorate(items), total, nil
}

// Attention returns the highest-priority work waiting for a person.
func (s *Service) Attention(limit int) ([]*TicketView, int, error) {
	items, _, err := s.st.ListTickets(store.TicketFilter{State: "open"}, 200, 0)
	if err != nil {
		return nil, 0, err
	}
	views := s.decorate(items)
	out := make([]*TicketView, 0, len(views))
	for _, v := range views {
		if v.Attention {
			out = append(out, v)
		}
	}
	sortViews(out)
	total := len(out)
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// AttentionPage is one round-trip answer for the console's attention view:
// the items, their total, and the effective per-stage counts. Assembling it in
// the work service keeps the gateway out of ticket business logic.
type AttentionPage struct {
	Items  []*TicketView  `json:"items"`
	Total  int            `json:"total"`
	Counts map[string]int `json:"counts"`
}

// AttentionPage returns the highest-priority work waiting for a person plus
// the stage counts for the badges.
func (s *Service) AttentionPage(limit int) (*AttentionPage, error) {
	items, total, err := s.Attention(limit)
	if err != nil {
		return nil, err
	}
	counts, err := s.StageCounts()
	if err != nil {
		return nil, err
	}
	return &AttentionPage{Items: items, Total: total, Counts: counts}, nil
}

// StageCounts returns effective stage counts (presence + lease overlays).
func (s *Service) StageCounts() (map[string]int, error) {
	return s.st.TicketStageCounts(s.now(), s.onlineSecs)
}

// Get returns one ticket with the same presence/assignment decoration as List.
func (s *Service) Get(id int64) (*TicketView, error) {
	t, err := s.st.GetTicket(id)
	if err != nil || t == nil {
		return nil, err
	}
	v := s.decorate([]*store.Ticket{t})
	if len(v) == 0 {
		return nil, nil
	}
	return v[0], nil
}

// Subscribe adds a context/own subscription for the ticket's assignee. The
// privacy rule is enforced here as well as in the delivery query: the peer must
// already be a participant in the thread.
func (s *Service) Subscribe(id int64, peerID, threadRoot, mode string) (*TicketView, error) {
	t, err := s.st.GetTicket(id)
	if err != nil || t == nil {
		return nil, err
	}
	if t.Assignee != peerID {
		return nil, fmt.Errorf("ticket is assigned to another peer")
	}
	ok, err := s.st.PeerInThread(threadRoot, peerID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("peer is not a participant in thread %s", threadRoot)
	}
	if err := s.st.SubscribeTicket(id, threadRoot, mode, s.now()); err != nil {
		return nil, err
	}
	return s.Get(id)
}

// Reassign ends the current assignment, starts a new pending one, and refreshes
// the projection. The ticket id (the opening task seq) never changes.
func (s *Service) Reassign(id int64, assignee string, leaseSecs int64) (*TicketView, error) {
	if t, err := s.st.GetTicket(id); err != nil {
		return nil, err
	} else if t == nil {
		return nil, nil
	}
	if _, err := s.st.ReassignTicket(store.AssignmentCommand{
		TicketID: id, Assignee: assignee, LeaseSecs: leaseSecs, Now: s.now(),
	}); err != nil {
		return nil, err
	}
	if err := s.refresh(id); err != nil {
		return nil, err
	}
	return s.Get(id)
}

func (s *Service) decorate(items []*store.Ticket) []*TicketView {
	now := s.now()
	out := make([]*TicketView, 0, len(items))
	for _, t := range items {
		assignments, _ := s.st.ListAssignments(t.ID)
		latest := (*store.TicketAssignment)(nil)
		if n := len(assignments); n > 0 {
			latest = assignments[n-1]
		}
		var current *store.TicketAssignment
		for i := len(assignments) - 1; i >= 0; i-- {
			if assignments[i].Status != store.AssignmentEnded {
				current = assignments[i]
				break
			}
		}
		leaseExpired := latest != nil && latest.Status == store.AssignmentEnded &&
			latest.EndedReason == store.AssignmentReasonLease
		stage, attention, online := EffectiveLease(t, leaseExpired, now, s.onlineSecs)
		v := &TicketView{
			Ticket: *t, EffectiveStage: stage, Attention: attention,
			AssigneeOnline: online, LeaseExpired: leaseExpired,
			Assignment: current, Assignments: assignments,
		}
		v.Subscriptions, _ = s.st.TicketSubscriptions(t.ID)
		v.Mandate, _ = s.st.GetMandate(t.ID)
		out = append(out, v)
	}
	return out
}

func sortViews(v []*TicketView) {
	sort.SliceStable(v, func(i, j int) bool {
		wi, wj := stages.Rank(v[i].EffectiveStage), stages.Rank(v[j].EffectiveStage)
		if wi != wj {
			return wi < wj
		}
		return v[i].UpdatedAt > v[j].UpdatedAt
	})
}

func pageViews(v []*TicketView, limit, offset int) []*TicketView {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(v) {
		return []*TicketView{}
	}
	end := offset + limit
	if end > len(v) {
		end = len(v)
	}
	return v[offset:end]
}

func (s *Service) Counts() (*store.TicketCounts, error) {
	return s.st.TicketCounts(s.now(), s.onlineSecs)
}

// Ready reports whether the initial rebuild finished.
func (s *Service) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ready
}

// LastError returns the most recent projection error, if any.
func (s *Service) LastError() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastErr
}

func (s *Service) setErr(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

func requesterKind(sender string) string {
	switch sender {
	case "operator":
		return "operator"
	case "system":
		return "system"
	default:
		return "peer"
	}
}

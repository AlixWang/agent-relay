package resources

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Self-service resource registration (ADR 0006): peers submit resources into
// a staging table; an operator reviews and approves them into the live hub.

var (
	ErrDisabled             = errors.New("resource self-registration is disabled")
	ErrNameTaken            = errors.New("resource name already exists")
	ErrQuota                = errors.New("resource quota exceeded")
	ErrRateLimited          = errors.New("resource submission rate limited")
	ErrSubmissionNotPending = errors.New("resource submission is not pending")
	ErrTakedownExists       = errors.New("a takedown request is already pending")
)

// SubmissionPolicy holds the operator-controlled limits for self-registration.
type SubmissionPolicy struct {
	Enabled         bool
	PendingMax      int
	LiveMax         int
	PerHour         int
	PendingTTLDays  int
	NotifyBatchSecs int
}

// DefaultSubmissionPolicy mirrors config.Default() for tests and callers that
// construct a Service directly.
func DefaultSubmissionPolicy() SubmissionPolicy {
	return SubmissionPolicy{Enabled: true, PendingMax: 20, LiveMax: 100, PerHour: 10, PendingTTLDays: 30, NotifyBatchSecs: 600}
}

// SetSubmissionPolicy applies config.toml values. Zero values fall back to the
// documented defaults; Enabled is taken literally.
func (s *Service) SetSubmissionPolicy(p SubmissionPolicy) {
	d := DefaultSubmissionPolicy()
	if p.PendingMax <= 0 {
		p.PendingMax = d.PendingMax
	}
	if p.LiveMax <= 0 {
		p.LiveMax = d.LiveMax
	}
	if p.PerHour <= 0 {
		p.PerHour = d.PerHour
	}
	if p.PendingTTLDays < 0 {
		p.PendingTTLDays = d.PendingTTLDays
	}
	if p.NotifyBatchSecs < 0 {
		p.NotifyBatchSecs = d.NotifyBatchSecs
	}
	s.policy = p
}

// SubmissionPolicyOf returns the effective policy (used by the gateway for
// notification throttling and config display).
func (s *Service) SubmissionPolicyOf() SubmissionPolicy { return s.policy }

// SubmissionView is the safe wire form of a staged submission. It never
// carries content (secret plaintext or otherwise).
type SubmissionView struct {
	ID           string `json:"id"`
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	Hash         string `json:"hash"`
	SubmittedBy  string `json:"submitted_by"`
	Status       string `json:"status"`
	ReviewNote   string `json:"review_note,omitempty"`
	ReviewedBy   string `json:"reviewed_by,omitempty"`
	ReviewedAt   int64  `json:"reviewed_at,omitempty"`
	ApprovedName string `json:"approved_name,omitempty"`
	CreatedAt    int64  `json:"created_at"`
	UpdatedAt    int64  `json:"updated_at"`
}

// SubmissionDetail adds the reviewable body. Secret submissions never include
// content here; mcp/prompt include their body; skill includes a file listing.
type SubmissionDetail struct {
	SubmissionView
	Content         any              `json:"content,omitempty"`
	RequiresSecrets []string         `json:"requires_secrets,omitempty"`
	Skill           *SkillInspection `json:"skill,omitempty"`
}

// TakedownView is the safe wire form of a takedown request.
type TakedownView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	RequestedBy string `json:"requested_by"`
	Reason      string `json:"reason,omitempty"`
	Status      string `json:"status"`
	ReviewNote  string `json:"review_note,omitempty"`
	ReviewedBy  string `json:"reviewed_by,omitempty"`
	ReviewedAt  int64  `json:"reviewed_at,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

func toSubmissionView(sub *store.ResourceSubmission) *SubmissionView {
	if sub == nil {
		return nil
	}
	return &SubmissionView{
		ID: sub.ID, Kind: sub.Kind, Name: sub.Name, Description: sub.Description,
		Hash: sub.ContentHash, SubmittedBy: sub.SubmittedBy, Status: sub.Status,
		ReviewNote: sub.ReviewNote, ReviewedBy: sub.ReviewedBy, ReviewedAt: sub.ReviewedAt,
		ApprovedName: sub.ApprovedName, CreatedAt: sub.CreatedAt, UpdatedAt: sub.UpdatedAt,
	}
}

func toTakedownView(t *store.ResourceTakedown) *TakedownView {
	if t == nil {
		return nil
	}
	return &TakedownView{
		ID: t.ID, Kind: t.Kind, Name: t.Name, RequestedBy: t.RequestedBy, Reason: t.Reason,
		Status: t.Status, ReviewNote: t.ReviewNote, ReviewedBy: t.ReviewedBy, ReviewedAt: t.ReviewedAt,
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// SubmitSecret stages a secret. The value is encrypted with the hub key before
// it touches the database; the returned view only exposes the HMAC hash.
func (s *Service) SubmitSecret(peerID, name, description, value string) (*SubmissionView, bool, error) {
	if !ValidName(name) {
		return nil, false, fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
	}
	if value == "" {
		return nil, false, fmt.Errorf("%w: value required", ErrInvalid)
	}
	if len(value) > maxSecretBytes {
		return nil, false, fmt.Errorf("%w: value larger than %d bytes", ErrInvalid, maxSecretBytes)
	}
	if !utf8ValidNoNUL(value) {
		return nil, false, fmt.Errorf("%w: value must be UTF-8 text", ErrInvalid)
	}
	sealed, err := seal(s.key, []byte(value))
	if err != nil {
		return nil, false, err
	}
	return s.submitContent(peerID, KindSecret, name, description, sealed, secretHash(s.key, value))
}

// SubmitMCP stages a neutral MCP config. It must already use ${secret:NAME}
// references; the relay cannot tell an arbitrary literal token from ordinary
// config text, so the prompt requires assistants to split secrets first and
// the operator reviews the body before approval.
func (s *Service) SubmitMCP(peerID, name, description string, raw []byte) (*SubmissionView, bool, error) {
	canonical, _, err := ParseMCP(raw)
	if err != nil {
		return nil, false, err
	}
	return s.submitContent(peerID, KindMCP, name, description, canonical, contentHash(canonical))
}

// SubmitPrompt stages a prompt snippet.
func (s *Service) SubmitPrompt(peerID, name, description, text string) (*SubmissionView, bool, error) {
	if err := CheckPrompt(text); err != nil {
		return nil, false, err
	}
	return s.submitContent(peerID, KindPrompt, name, description, []byte(text), contentHash([]byte(text)))
}

// SubmitSkill stages a tar.gz skill; its name comes from SKILL.md frontmatter.
func (s *Service) SubmitSkill(peerID string, raw []byte, description string) (*SubmissionView, bool, error) {
	meta, err := ParseSkill(raw)
	if err != nil {
		return nil, false, err
	}
	if strings.TrimSpace(description) == "" {
		description = meta.Description
	}
	return s.submitContent(peerID, KindSkill, meta.Name, description, raw, contentHash(raw))
}

func utf8ValidNoNUL(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// submitContent implements the common validation/quota/duplicate path.
func (s *Service) submitContent(peerID, kind, name, description string, content []byte, hash string) (*SubmissionView, bool, error) {
	if !s.policy.Enabled {
		return nil, false, ErrDisabled
	}
	if !ValidName(name) {
		return nil, false, fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
	}
	desc, err := cleanDesc(description)
	if err != nil {
		return nil, false, err
	}
	now := s.now().Unix()
	// A peer may paste a live secret into the description by accident. Reuse
	// the message redactor; the audit row carries names only, never values.
	if masked, hits := s.Redactor().Redact(desc); masked != desc {
		desc = masked
		_ = s.st.AppendAudit(peerID, "secret.redacted",
			fmt.Sprintf("secret=%s where=resource_submission", strings.Join(hits, ",")), now)
	}

	// Live resource with this name: either an update by its owner, a
	// same-content no-op, or a name conflict.
	live, err := s.st.GetResource(kind, name)
	if err != nil {
		return nil, false, err
	}
	if live != nil {
		if live.OwnerPeer != peerID {
			return nil, false, ErrNameTaken
		}
		if live.ContentHash == hash {
			return toSubmissionView(&store.ResourceSubmission{
				ID: "", Kind: kind, Name: name, Description: desc, ContentHash: hash,
				SubmittedBy: peerID, Status: "unchanged", CreatedAt: now, UpdatedAt: now,
			}), true, nil
		}
	}

	// A pending row for the same owner/kind/name is replaced in place;
	// otherwise enforce the pending quota for a genuinely new pending row.
	pending, err := s.findPending(peerID, kind, name)
	if err != nil {
		return nil, false, err
	}
	if pending == nil {
		n, err := s.st.CountResourceSubmissions(peerID, "pending", 0)
		if err != nil {
			return nil, false, err
		}
		if n >= s.policy.PendingMax {
			return nil, false, ErrQuota
		}
	}
	// Live quota applies only when creating a brand-new resource, not when
	// updating an existing one.
	if live == nil {
		n, err := s.st.CountLiveResourcesOwnedBy(peerID)
		if err != nil {
			return nil, false, err
		}
		if n >= s.policy.LiveMax {
			return nil, false, ErrQuota
		}
	}
	// Per-hour rate limit counts every attempted submission, including
	// replacements, so reject/retry loops cannot bypass it.
	n, err := s.st.CountResourceSubmissions(peerID, "", now-3600)
	if err != nil {
		return nil, false, err
	}
	if n >= s.policy.PerHour {
		return nil, false, ErrRateLimited
	}

	if pending != nil {
		if err := s.st.ReplacePendingResourceSubmission(pending.ID, desc, content, hash, now); err != nil {
			return nil, false, err
		}
		pending.Description, pending.ContentHash, pending.UpdatedAt = desc, hash, now
		return toSubmissionView(pending), false, nil
	}

	sub := &store.ResourceSubmission{
		ID: newSubmissionID(), Kind: kind, Name: name, Description: desc,
		Content: content, ContentHash: hash, SubmittedBy: peerID, Status: "pending",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateResourceSubmission(sub); err != nil {
		return nil, false, err
	}
	return toSubmissionView(sub), false, nil
}

func (s *Service) findPending(peerID, kind, name string) (*store.ResourceSubmission, error) {
	subs, _, err := s.st.ListResourceSubmissions(store.ResourceSubmissionFilter{
		Status: "pending", Kind: kind, Peer: peerID,
	})
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if sub.Name == name {
			return sub, nil
		}
	}
	return nil, nil
}

// ListMine returns a peer's own submissions, newest first.
func (s *Service) ListMine(peerID, status string, limit, offset int) ([]*SubmissionView, int, error) {
	if status != "" && !validSubmissionStatus(status) {
		return nil, 0, fmt.Errorf("%w: bad status", ErrInvalid)
	}
	subs, total, err := s.st.ListResourceSubmissions(store.ResourceSubmissionFilter{
		Status: status, Peer: peerID, Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, 0, err
	}
	out := make([]*SubmissionView, 0, len(subs))
	for _, sub := range subs {
		out = append(out, toSubmissionView(sub))
	}
	return out, total, nil
}

// AdminList returns all submissions for the review console.
func (s *Service) AdminList(filter store.ResourceSubmissionFilter) ([]*SubmissionView, int, error) {
	if filter.Status != "" && !validSubmissionStatus(filter.Status) {
		return nil, 0, fmt.Errorf("%w: bad status", ErrInvalid)
	}
	if filter.Kind != "" && !knownKind(filter.Kind) {
		return nil, 0, fmt.Errorf("%w: bad kind", ErrInvalid)
	}
	subs, total, err := s.st.ListResourceSubmissions(filter)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*SubmissionView, 0, len(subs))
	for _, sub := range subs {
		out = append(out, toSubmissionView(sub))
	}
	return out, total, nil
}

// GetSubmissionDetail returns one submission with reviewable body metadata.
// Secret content is never returned.
func (s *Service) GetSubmissionDetail(id string) (*SubmissionDetail, error) {
	sub, err := s.st.GetResourceSubmission(id)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, ErrNotFound
	}
	d := &SubmissionDetail{SubmissionView: *toSubmissionView(sub)}
	switch sub.Kind {
	case KindMCP:
		d.Content, d.RequiresSecrets = decodeContent(sub.Kind, sub.Content)
	case KindPrompt:
		d.Content, _ = decodeContent(sub.Kind, sub.Content)
	case KindSkill:
		if len(sub.Content) > 0 {
			insp, err := InspectSkill(sub.Content)
			if err != nil {
				return nil, err
			}
			d.Skill = insp
		}
	}
	return d, nil
}

// Withdraw lets a peer withdraw its own pending submission. A missing or
// non-pending row is a 404 by design.
func (s *Service) Withdraw(peerID, id string) error {
	sub, err := s.st.GetResourceSubmission(id)
	if err != nil {
		return err
	}
	if sub == nil || sub.SubmittedBy != peerID || sub.Status != "pending" {
		return ErrNotFound
	}
	ok, err := s.st.UpdateResourceSubmissionStatus(id, "withdrawn", "withdrawn by source", peerID, s.now().Unix(), true)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// Approve turns one pending submission into a live resource revision.
func (s *Service) Approve(id, reviewer, nameOverride, descriptionOverride string) (*View, error) {
	sub, err := s.st.GetResourceSubmission(id)
	if err != nil {
		return nil, err
	}
	if sub == nil {
		return nil, ErrNotFound
	}
	if sub.Status != "pending" {
		return nil, ErrSubmissionNotPending
	}
	name := sub.Name
	if strings.TrimSpace(nameOverride) != "" {
		name = strings.TrimSpace(nameOverride)
		if !ValidName(name) {
			return nil, fmt.Errorf("%w: name must match %s", ErrInvalid, nameRe)
		}
	}
	if sub.Kind == KindSkill && name != sub.Name {
		return nil, fmt.Errorf("%w: skill name must come from SKILL.md", ErrInvalid)
	}
	desc := sub.Description
	if strings.TrimSpace(descriptionOverride) != "" {
		d, err := cleanDesc(descriptionOverride)
		if err != nil {
			return nil, err
		}
		desc = d
	}
	revID := newRevisionID()
	res, err := s.st.ApproveResourceSubmission(id, revID, name, desc, reviewer, s.now().Unix())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrSubmissionNotFound):
			return nil, ErrNotFound
		case errors.Is(err, store.ErrSubmissionNotPending):
			return nil, ErrSubmissionNotPending
		case errors.Is(err, store.ErrResourceNameTaken):
			return nil, ErrNameTaken
		default:
			return nil, err
		}
	}
	if res.Kind == KindSecret {
		s.Redactor().invalidate()
	}
	return s.Get(res.Kind, res.Name)
}

// Reject closes a pending submission and wipes its content.
func (s *Service) Reject(id, reviewer, note string) error {
	note, err := cleanDesc(note)
	if err != nil {
		return err
	}
	ok, err := s.st.UpdateResourceSubmissionStatus(id, "rejected", note, reviewer, s.now().Unix(), true)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// ExpirePendingSubmissions marks stale pending rows expired and wipes them.
func (s *Service) ExpirePendingSubmissions() (int64, error) {
	p := s.policy.PendingTTLDays
	if p <= 0 {
		return 0, nil
	}
	now := s.now().Unix()
	return s.st.ExpirePendingSubmissions(now-int64(p)*86400, now)
}

// PendingCounts returns the number of pending submissions and takedowns for
// the console badge.
func (s *Service) PendingCounts() (int, int, error) {
	_, subs, err := s.st.ListResourceSubmissions(store.ResourceSubmissionFilter{Status: "pending", Limit: 1})
	if err != nil {
		return 0, 0, err
	}
	_, tds, err := s.st.ListResourceTakedowns("pending", "", 1, 0)
	if err != nil {
		return 0, 0, err
	}
	return subs, tds, nil
}

// RequestTakedown lets the owner of a live resource ask the operator to remove
// it. The resource keeps serving until the operator approves.
func (s *Service) RequestTakedown(peerID, kind, name, reason string) (*TakedownView, error) {
	if !knownKind(kind) {
		return nil, ErrNotFound
	}
	res, err := s.st.GetResource(kind, name)
	if err != nil {
		return nil, err
	}
	if res == nil || res.OwnerPeer != peerID {
		return nil, ErrNotFound
	}
	has, err := s.st.HasPendingTakedown(kind, name)
	if err != nil {
		return nil, err
	}
	if has {
		return nil, ErrTakedownExists
	}
	reason, err = cleanDesc(reason)
	if err != nil {
		return nil, err
	}
	now := s.now().Unix()
	t := &store.ResourceTakedown{
		ID: newTakedownID(), Kind: kind, Name: name, RequestedBy: peerID,
		Reason: reason, Status: "pending", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.st.CreateResourceTakedown(t); err != nil {
		return nil, err
	}
	return toTakedownView(t), nil
}

// ListMineTakedowns returns a peer's own takedown requests.
func (s *Service) ListMineTakedowns(peerID string, limit, offset int) ([]*TakedownView, int, error) {
	ts, total, err := s.st.ListResourceTakedowns("", peerID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*TakedownView, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTakedownView(t))
	}
	return out, total, nil
}

// AdminListTakedowns lists takedown requests for the console.
func (s *Service) AdminListTakedowns(status string, limit, offset int) ([]*TakedownView, int, error) {
	ts, total, err := s.st.ListResourceTakedowns(status, "", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*TakedownView, 0, len(ts))
	for _, t := range ts {
		out = append(out, toTakedownView(t))
	}
	return out, total, nil
}

// ResolveTakedown approves (deletes the live resource) or rejects a request.
func (s *Service) ResolveTakedown(id, reviewer string, approve bool, note string) error {
	t, err := s.st.GetResourceTakedown(id)
	if err != nil {
		return err
	}
	if t == nil {
		return ErrNotFound
	}
	if t.Status != "pending" {
		return ErrTakedownExists
	}
	note, err = cleanDesc(note)
	if err != nil {
		return err
	}
	if approve {
		ok, err := s.st.DeleteResource(t.Kind, t.Name)
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		if t.Kind == KindSecret {
			s.Redactor().invalidate()
		}
	}
	status := "rejected"
	if approve {
		status = "approved"
	}
	ok, err := s.st.ResolveResourceTakedown(id, status, note, reviewer, s.now().Unix())
	if err != nil {
		return err
	}
	if !ok {
		return ErrNotFound
	}
	return nil
}

// SubmissionSkillArchive returns a pending/approved skill submission's raw
// archive for the admin review UI.
func (s *Service) SubmissionSkillArchive(id string) ([]byte, error) {
	sub, err := s.st.GetResourceSubmission(id)
	if err != nil {
		return nil, err
	}
	if sub == nil || sub.Kind != KindSkill || len(sub.Content) == 0 {
		return nil, ErrNotFound
	}
	return sub.Content, nil
}

// LiveSkillArchive returns a live skill's bytes for admin review.
func (s *Service) LiveSkillArchive(name string) ([]byte, error) {
	r, err := s.st.GetResource(KindSkill, name)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, ErrNotFound
	}
	return r.Content, nil
}

func validSubmissionStatus(s string) bool {
	switch s {
	case "pending", "approved", "rejected", "withdrawn", "expired", "superseded":
		return true
	}
	return false
}

func newSubmissionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("rsub_%d", time.Now().UnixNano())
	}
	return "rsub_" + hex.EncodeToString(b[:])
}

func newTakedownID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("rtd_%d", time.Now().UnixNano())
	}
	return "rtd_" + hex.EncodeToString(b[:])
}

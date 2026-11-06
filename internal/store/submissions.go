package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Resource submission and takedown persistence (ADR 0006). Business rules
// live in internal/resources; this file only knows rows and transactions.

var (
	ErrSubmissionNotFound   = errors.New("resource submission not found")
	ErrSubmissionNotPending = errors.New("resource submission not pending")
	ErrResourceNameTaken    = errors.New("resource name already exists")
	ErrTakedownNotFound     = errors.New("resource takedown not found")
	ErrTakedownNotPending   = errors.New("resource takedown not pending")
)

// ResourceSubmission is one staged resource contribution from a peer.
type ResourceSubmission struct {
	ID           string
	Kind         string
	Name         string
	Description  string
	Content      []byte
	ContentHash  string
	SubmittedBy  string
	Status       string
	ReviewNote   string
	ReviewedBy   string
	ReviewedAt   int64
	ApprovedName string
	RevisionID   string
	CreatedAt    int64
	UpdatedAt    int64
}

// ResourceTakedown is a peer's request to remove one of its live resources.
type ResourceTakedown struct {
	ID          string
	Kind        string
	Name        string
	RequestedBy string
	Reason      string
	Status      string
	ReviewNote  string
	ReviewedBy  string
	ReviewedAt  int64
	CreatedAt   int64
	UpdatedAt   int64
}

// ResourceSubmissionFilter selects submissions for the admin console or a
// peer's "mine" view. Empty fields mean "no filter".
type ResourceSubmissionFilter struct {
	Status string
	Kind   string
	Peer   string
	Q      string
	Limit  int
	Offset int
}

const submissionMetaColumns = `id,kind,name,description,content_hash,submitted_by,status,review_note,reviewed_by,reviewed_at,approved_name,revision_id,created_at,updated_at`

func scanSubmissionMeta(sc rowScanner) (*ResourceSubmission, error) {
	var s ResourceSubmission
	if err := sc.Scan(&s.ID, &s.Kind, &s.Name, &s.Description, &s.ContentHash,
		&s.SubmittedBy, &s.Status, &s.ReviewNote, &s.ReviewedBy, &s.ReviewedAt,
		&s.ApprovedName, &s.RevisionID, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

func scanSubmission(sc rowScanner) (*ResourceSubmission, error) {
	var s ResourceSubmission
	if err := sc.Scan(&s.ID, &s.Kind, &s.Name, &s.Description, &s.Content, &s.ContentHash,
		&s.SubmittedBy, &s.Status, &s.ReviewNote, &s.ReviewedBy, &s.ReviewedAt,
		&s.ApprovedName, &s.RevisionID, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	return &s, nil
}

// CreateResourceSubmission inserts a pending submission. The partial unique
// index on (submitted_by,kind,name) WHERE status='pending' is the final guard
// against two simultaneous pending rows; callers normally replace the pending
// row explicitly instead of relying on this error.
func (s *sqliteStore) CreateResourceSubmission(sub *ResourceSubmission) error {
	_, err := s.db.Exec(`INSERT INTO resource_submissions(id,kind,name,description,content,content_hash,
		submitted_by,status,review_note,reviewed_by,reviewed_at,approved_name,revision_id,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		sub.ID, sub.Kind, sub.Name, sub.Description, sub.Content, sub.ContentHash,
		sub.SubmittedBy, sub.Status, sub.ReviewNote, sub.ReviewedBy, sub.ReviewedAt,
		sub.ApprovedName, sub.RevisionID, sub.CreatedAt, sub.UpdatedAt)
	return err
}

func (s *sqliteStore) GetResourceSubmission(id string) (*ResourceSubmission, error) {
	return s.getResourceSubmission(id)
}

func (s *sqliteStore) getResourceSubmission(id string) (*ResourceSubmission, error) {
	var sub ResourceSubmission
	err := s.db.QueryRow(`SELECT id,kind,name,description,content,content_hash,submitted_by,status,
		review_note,reviewed_by,reviewed_at,approved_name,revision_id,created_at,updated_at
		FROM resource_submissions WHERE id=?`, id).
		Scan(&sub.ID, &sub.Kind, &sub.Name, &sub.Description, &sub.Content, &sub.ContentHash,
			&sub.SubmittedBy, &sub.Status, &sub.ReviewNote, &sub.ReviewedBy, &sub.ReviewedAt,
			&sub.ApprovedName, &sub.RevisionID, &sub.CreatedAt, &sub.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

func (s *sqliteStore) ListResourceSubmissions(f ResourceSubmissionFilter) ([]*ResourceSubmission, int, error) {
	where, args := submissionWhere(f)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM resource_submissions`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit, offset := f.Limit, f.Offset
	if limit <= 0 {
		limit = -1
	}
	if offset < 0 {
		offset = 0
	}
	qargs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.Query(`SELECT `+submissionMetaColumns+` FROM resource_submissions`+where+
		` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, qargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*ResourceSubmission
	for rows.Next() {
		sub, err := scanSubmissionMeta(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, sub)
	}
	return out, total, rows.Err()
}

func submissionWhere(f ResourceSubmissionFilter) (string, []any) {
	clauses := []string{"1=1"}
	args := []any{}
	if f.Status != "" {
		clauses = append(clauses, "status=?")
		args = append(args, f.Status)
	}
	if f.Kind != "" {
		clauses = append(clauses, "kind=?")
		args = append(args, f.Kind)
	}
	if f.Peer != "" {
		clauses = append(clauses, "submitted_by=?")
		args = append(args, f.Peer)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		like := "%" + escapeLike(q) + "%"
		clauses = append(clauses, `(name LIKE ? ESCAPE '\' OR description LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// UpdateResourceSubmissionStatus changes a non-approved state. It wipes the
// content when wipe is true (reject/withdraw/expire/supersede).
func (s *sqliteStore) UpdateResourceSubmissionStatus(id, status, note, reviewer string, reviewedAt int64, wipe bool) (bool, error) {
	content := "content=content"
	if wipe {
		content = "content=x''"
	}
	res, err := s.db.Exec(`UPDATE resource_submissions SET status=?, review_note=?, reviewed_by=?, reviewed_at=?,
		updated_at=?, `+content+` WHERE id=? AND status='pending'`,
		status, note, reviewer, reviewedAt, reviewedAt, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ReplacePendingResourceSubmission updates the body of an existing pending
// row (same owner/kind/name re-submission).
func (s *sqliteStore) ReplacePendingResourceSubmission(id, description string, content []byte, contentHash string, now int64) error {
	_, err := s.db.Exec(`UPDATE resource_submissions SET description=?, content=?, content_hash=?, updated_at=?
		WHERE id=? AND status='pending'`, description, content, contentHash, now, id)
	return err
}

// SupersedePendingResourceSubmission marks an older pending row as superseded
// and wipes its content.
func (s *sqliteStore) SupersedePendingResourceSubmission(id string, now int64) error {
	_, err := s.db.Exec(`UPDATE resource_submissions SET status='superseded', review_note='superseded by a newer submission',
		reviewed_by='system', reviewed_at=?, updated_at=?, content=x'' WHERE id=? AND status='pending'`, now, now, id)
	return err
}

// ExpirePendingSubmissions expires pending rows older than olderThan and wipes
// their content. Returns the number expired.
func (s *sqliteStore) ExpirePendingSubmissions(olderThan, now int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE resource_submissions SET status='expired', review_note='submission expired',
		reviewed_by='system', reviewed_at=?, updated_at=?, content=x'' WHERE status='pending' AND created_at<?`,
		now, now, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountResourceSubmissions counts a peer's submissions with a given status
// (empty = any) created at or after since (0 = all time).
func (s *sqliteStore) CountResourceSubmissions(peerID, status string, since int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM resource_submissions
		WHERE submitted_by=? AND (?='' OR status=?) AND created_at>=?`, peerID, status, status, since).Scan(&n)
	return n, err
}

// CountLiveResourcesOwnedBy counts live resources whose owner is peerID.
func (s *sqliteStore) CountLiveResourcesOwnedBy(peerID string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM resources WHERE owner_peer=?`, peerID).Scan(&n)
	return n, err
}

// ApproveResourceSubmission atomically turns a pending submission into a
// live resource revision. approvedName may be empty to keep the submitted
// name; description is the final description (the service applies overrides).
// The caller must supply a fresh revision ID and timestamp.
func (s *sqliteStore) ApproveResourceSubmission(id, revisionID, approvedName, description, reviewer string, now int64) (*Resource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sub ResourceSubmission
	err = tx.QueryRow(`SELECT id,kind,name,description,content,content_hash,submitted_by,status
		FROM resource_submissions WHERE id=?`, id).
		Scan(&sub.ID, &sub.Kind, &sub.Name, &sub.Description, &sub.Content, &sub.ContentHash, &sub.SubmittedBy, &sub.Status)
	if err == sql.ErrNoRows {
		return nil, ErrSubmissionNotFound
	}
	if err != nil {
		return nil, err
	}
	if sub.Status != "pending" {
		return nil, ErrSubmissionNotPending
	}
	name := strings.TrimSpace(approvedName)
	if name == "" {
		name = sub.Name
	}
	if description == "" {
		description = sub.Description
	}

	var currentOwner string
	err = tx.QueryRow(`SELECT owner_peer FROM resources WHERE kind=? AND name=?`, sub.Kind, name).Scan(&currentOwner)
	switch {
	case err == sql.ErrNoRows:
		// New resource: fall through to insert.
	case err != nil:
		return nil, err
	case currentOwner != sub.SubmittedBy:
		return nil, ErrResourceNameTaken
	}

	createdBy := "peer:" + sub.SubmittedBy
	if _, err := tx.Exec(`INSERT INTO resource_revisions(id,kind,name,description,content,content_hash,created_at,created_by)
		VALUES(?,?,?,?,?,?,?,?)`, revisionID, sub.Kind, name, description, sub.Content, sub.ContentHash, now, createdBy); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`INSERT INTO resources(kind,name,revision_id,updated_at,owner_peer) VALUES(?,?,?,?,?)
		ON CONFLICT(kind,name) DO UPDATE SET revision_id=excluded.revision_id, updated_at=excluded.updated_at`,
		sub.Kind, name, revisionID, now, sub.SubmittedBy); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE resource_submissions SET status='approved', reviewed_by=?, reviewed_at=?, approved_name=?,
		revision_id=?, review_note='', updated_at=? WHERE id=? AND status='pending'`,
		reviewer, now, name, revisionID, now, id); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &Resource{
		ResourceRevision: ResourceRevision{
			ID: revisionID, Kind: sub.Kind, Name: name, Description: description,
			Content: sub.Content, ContentHash: sub.ContentHash, CreatedAt: now, CreatedBy: createdBy,
		},
		UpdatedAt: now, OwnerPeer: sub.SubmittedBy,
	}, nil
}

// ResourceTakedown persistence -------------------------------------------------

func (s *sqliteStore) CreateResourceTakedown(t *ResourceTakedown) error {
	_, err := s.db.Exec(`INSERT INTO resource_takedowns(id,kind,name,requested_by,reason,status,review_note,
		reviewed_by,reviewed_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Kind, t.Name, t.RequestedBy, t.Reason, t.Status, t.ReviewNote,
		t.ReviewedBy, t.ReviewedAt, t.CreatedAt, t.UpdatedAt)
	return err
}

func (s *sqliteStore) GetResourceTakedown(id string) (*ResourceTakedown, error) {
	var t ResourceTakedown
	err := s.db.QueryRow(`SELECT id,kind,name,requested_by,reason,status,review_note,reviewed_by,reviewed_at,created_at,updated_at
		FROM resource_takedowns WHERE id=?`, id).
		Scan(&t.ID, &t.Kind, &t.Name, &t.RequestedBy, &t.Reason, &t.Status, &t.ReviewNote,
			&t.ReviewedBy, &t.ReviewedAt, &t.CreatedAt, &t.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *sqliteStore) HasPendingTakedown(kind, name string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM resource_takedowns WHERE kind=? AND name=? AND status='pending'`, kind, name).Scan(&n)
	return n > 0, err
}

func (s *sqliteStore) ListResourceTakedowns(status, peer string, limit, offset int) ([]*ResourceTakedown, int, error) {
	where := " WHERE 1=1"
	args := []any{}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	if peer != "" {
		where += " AND requested_by=?"
		args = append(args, peer)
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM resource_takedowns`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = -1
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := s.db.Query(`SELECT id,kind,name,requested_by,reason,status,review_note,reviewed_by,reviewed_at,created_at,updated_at
		FROM resource_takedowns`+where+` ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*ResourceTakedown
	for rows.Next() {
		var t ResourceTakedown
		if err := rows.Scan(&t.ID, &t.Kind, &t.Name, &t.RequestedBy, &t.Reason, &t.Status, &t.ReviewNote,
			&t.ReviewedBy, &t.ReviewedAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, &t)
	}
	return out, total, rows.Err()
}

func (s *sqliteStore) ResolveResourceTakedown(id, status, note, reviewer string, now int64) (bool, error) {
	res, err := s.db.Exec(`UPDATE resource_takedowns SET status=?, review_note=?, reviewed_by=?, reviewed_at=?, updated_at=?
		WHERE id=? AND status='pending'`, status, note, reviewer, now, now, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetPeerResourceSubmitEnabled toggles a peer's submission permission.
func (s *sqliteStore) SetPeerResourceSubmitEnabled(peerID string, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE peers SET resource_submit_enabled=? WHERE id=?`, v, peerID)
	return err
}

// TransferPeerResourceOwnership moves all live resources owned by peerID to
// the operator (owner_peer=”). It also withdraws the peer's pending
// submissions and wipes their content.
func (s *sqliteStore) TransferPeerResourceOwnership(peerID string, now int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE resources SET owner_peer='' WHERE owner_peer=?`, peerID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`UPDATE resource_submissions SET status='withdrawn', review_note='source peer deleted',
		reviewed_by='system', reviewed_at=?, updated_at=?, content=x'' WHERE submitted_by=? AND status='pending'`,
		now, now, peerID); err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// DeletePeerWithResourcePolicy deletes a peer. When deleteResources is true
// its live resources and their grants are deleted; otherwise ownership moves
// to the operator. Pending submissions always become withdrawn.
func (s *sqliteStore) DeletePeerWithResourcePolicy(id string, ts int64, deleteResources bool) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE tokens SET revoked_at=? WHERE peer_id=? AND revoked_at=0`, ts, id)
	if err != nil {
		return 0, err
	}
	revoked, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM verify_state WHERE peer_id=?`, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM muse_accounts WHERE peer_id=?`, id); err != nil {
		return 0, err
	}
	if deleteResources {
		if _, err := tx.Exec(`DELETE FROM resource_grants WHERE EXISTS
			(SELECT 1 FROM resources WHERE resources.kind=resource_grants.kind AND resources.name=resource_grants.name AND resources.owner_peer=?)`, id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`DELETE FROM resources WHERE owner_peer=?`, id); err != nil {
			return 0, err
		}
	} else {
		if _, err := tx.Exec(`UPDATE resources SET owner_peer='' WHERE owner_peer=?`, id); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`UPDATE resource_submissions SET status='withdrawn', review_note='source peer deleted',
		reviewed_by='system', reviewed_at=?, updated_at=?, content=x'' WHERE submitted_by=? AND status='pending'`,
		ts, ts, id); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM resource_grants WHERE peer_id=?`, id); err != nil {
		return 0, err
	}
	res, err = tx.Exec(`DELETE FROM peers WHERE id=?`, id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("unknown peer %q", id)
	}
	return revoked, tx.Commit()
}

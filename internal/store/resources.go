package store

import (
	"database/sql"
	"fmt"
)

// Resource hub (DESIGN §4.12, ADR 0004/0005). The store treats content as
// opaque bytes: secret ciphertext and hashing belong to internal/resources.

// ResourceRevision is one immutable version of a resource.
type ResourceRevision struct {
	ID          string
	Kind        string
	Name        string
	Description string
	Content     []byte
	ContentHash string
	CreatedAt   int64
	CreatedBy   string
}

// Resource is a live (kind,name) joined with its current revision.
type Resource struct {
	ResourceRevision
	UpdatedAt int64
}

// ResourceGrant authorizes one peer (or '*' for every peer) to use a resource.
type ResourceGrant struct {
	Kind      string
	Name      string
	PeerID    string
	GrantedAt int64
}

const resourceColumns = `v.id,v.kind,v.name,v.description,v.content,v.content_hash,v.created_at,v.created_by,r.updated_at`

func scanResource(sc rowScanner) (*Resource, error) {
	var r Resource
	err := sc.Scan(&r.ID, &r.Kind, &r.Name, &r.Description, &r.Content, &r.ContentHash,
		&r.CreatedAt, &r.CreatedBy, &r.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// PutResource appends a revision and points (kind,name) at it, in one transaction.
func (s *sqliteStore) PutResource(rev *ResourceRevision) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO resource_revisions(id,kind,name,description,content,content_hash,created_at,created_by)
		VALUES(?,?,?,?,?,?,?,?)`, rev.ID, rev.Kind, rev.Name, rev.Description, rev.Content, rev.ContentHash,
		rev.CreatedAt, rev.CreatedBy); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO resources(kind,name,revision_id,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(kind,name) DO UPDATE SET revision_id=excluded.revision_id, updated_at=excluded.updated_at`,
		rev.Kind, rev.Name, rev.ID, rev.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *sqliteStore) GetResource(kind, name string) (*Resource, error) {
	r, err := scanResource(s.db.QueryRow(`SELECT `+resourceColumns+`
		FROM resources r JOIN resource_revisions v ON v.id=r.revision_id
		WHERE r.kind=? AND r.name=?`, kind, name))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListResources returns live resources ordered by (kind,name). kind "" = all.
func (s *sqliteStore) ListResources(kind string) ([]*Resource, error) {
	rows, err := s.db.Query(`SELECT `+resourceColumns+`
		FROM resources r JOIN resource_revisions v ON v.id=r.revision_id
		WHERE ?='' OR r.kind=? ORDER BY r.kind, r.name`, kind, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListGrantedResources returns the live resources granted to peerID directly
// or through '*', ordered by (kind,name). Each resource appears once.
func (s *sqliteStore) ListGrantedResources(peerID string) ([]*Resource, error) {
	rows, err := s.db.Query(`SELECT `+resourceColumns+`
		FROM resources r JOIN resource_revisions v ON v.id=r.revision_id
		WHERE EXISTS (SELECT 1 FROM resource_grants g
			WHERE g.kind=r.kind AND g.name=r.name AND (g.peer_id=? OR g.peer_id='*'))
		ORDER BY r.kind, r.name`, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Resource
	for rows.Next() {
		r, err := scanResource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteResource drops the live pointer and every grant; revisions stay as
// history. Returns false when (kind,name) did not exist.
func (s *sqliteStore) DeleteResource(kind, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM resources WHERE kind=? AND name=?`, kind, name)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM resource_grants WHERE kind=? AND name=?`, kind, name); err != nil {
		return false, err
	}
	return n > 0, tx.Commit()
}

// ListResourceRevisions returns a resource's history, newest first. Content
// is not loaded: history is for display, and secret ciphertext stays put.
func (s *sqliteStore) ListResourceRevisions(kind, name string) ([]*ResourceRevision, error) {
	rows, err := s.db.Query(`SELECT id,kind,name,description,content_hash,created_at,created_by
		FROM resource_revisions WHERE kind=? AND name=? ORDER BY created_at DESC, rowid DESC`, kind, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ResourceRevision
	for rows.Next() {
		var r ResourceRevision
		if err := rows.Scan(&r.ID, &r.Kind, &r.Name, &r.Description, &r.ContentHash, &r.CreatedAt, &r.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// GrantResource is idempotent. The resource must exist.
func (s *sqliteStore) GrantResource(kind, name, peerID string, ts int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO resource_grants(kind,name,peer_id,granted_at)
		SELECT ?,?,?,? WHERE EXISTS (SELECT 1 FROM resources WHERE kind=? AND name=?)
		ON CONFLICT(kind,name,peer_id) DO NOTHING`, kind, name, peerID, ts, kind, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM resources WHERE kind=? AND name=?`, kind, name).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("unknown resource %s/%s", kind, name)
		}
	}
	return nil
}

// RevokeResource removes one grant; false when it did not exist.
func (s *sqliteStore) RevokeResource(kind, name, peerID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM resource_grants WHERE kind=? AND name=? AND peer_id=?`, kind, name, peerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListResourceGrants returns grants for one resource (kind/name set) or all
// grants (both empty), ordered for stable display.
func (s *sqliteStore) ListResourceGrants(kind, name string) ([]*ResourceGrant, error) {
	rows, err := s.db.Query(`SELECT kind,name,peer_id,granted_at FROM resource_grants
		WHERE (?='' OR kind=?) AND (?='' OR name=?) ORDER BY kind, name, peer_id`, kind, kind, name, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ResourceGrant
	for rows.Next() {
		var g ResourceGrant
		if err := rows.Scan(&g.Kind, &g.Name, &g.PeerID, &g.GrantedAt); err != nil {
			return nil, err
		}
		out = append(out, &g)
	}
	return out, rows.Err()
}

// UpdatePeerResourcesRev records the manifest digest a peer last applied.
func (s *sqliteStore) UpdatePeerResourcesRev(id, rev string, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET resources_rev=?, resources_updated_at=? WHERE id=?`, rev, ts, id)
	return err
}

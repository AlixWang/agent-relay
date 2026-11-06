package store

import (
	"errors"
	"testing"
)

func newSubmissionStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{"alice", "bob"} {
		if err := st.CreatePeer(&Peer{ID: id, AgentType: "muse", Status: "active", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func TestStoreResourceSubmissionApproveAndOwnership(t *testing.T) {
	st := newSubmissionStore(t)
	sub := &ResourceSubmission{
		ID: "rsub_1", Kind: "secret", Name: "K", Description: "desc", Content: []byte("cipher"),
		ContentHash: "hmac:x", SubmittedBy: "alice", Status: "pending", CreatedAt: 10, UpdatedAt: 10,
	}
	if err := st.CreateResourceSubmission(sub); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetResourceSubmission("rsub_1")
	if err != nil || got == nil || got.Status != "pending" {
		t.Fatalf("get: %+v %v", got, err)
	}
	res, err := st.ApproveResourceSubmission("rsub_1", "rr_1", "", "", "admin", 20)
	if err != nil || res.OwnerPeer != "alice" || res.Name != "K" {
		t.Fatalf("approve: %+v %v", res, err)
	}
	got, _ = st.GetResourceSubmission("rsub_1")
	if got.Status != "approved" || got.ApprovedName != "K" || got.RevisionID != "rr_1" {
		t.Fatalf("submission after approve: %+v", got)
	}
	if _, err := st.ApproveResourceSubmission("rsub_1", "rr_2", "", "", "admin", 30); !errors.Is(err, ErrSubmissionNotPending) {
		t.Fatalf("double approve: %v", err)
	}
}

func TestStoreResourceSubmissionNameConflict(t *testing.T) {
	st := newSubmissionStore(t)
	if err := st.PutResource(&ResourceRevision{ID: "rr_op", Kind: "secret", Name: "K", Content: []byte("x"), ContentHash: "h", CreatedAt: 1, CreatedBy: "operator"}, ""); err != nil {
		t.Fatal(err)
	}
	sub := &ResourceSubmission{ID: "rsub_2", Kind: "secret", Name: "K", Content: []byte("y"), ContentHash: "h2", SubmittedBy: "alice", Status: "pending", CreatedAt: 2, UpdatedAt: 2}
	if err := st.CreateResourceSubmission(sub); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ApproveResourceSubmission("rsub_2", "rr_2", "", "", "admin", 3); !errors.Is(err, ErrResourceNameTaken) {
		t.Fatalf("expected name taken: %v", err)
	}
}

func TestStoreDeletePeerResourcePolicy(t *testing.T) {
	st := newSubmissionStore(t)
	if err := st.PutResource(&ResourceRevision{ID: "rr_a", Kind: "prompt", Name: "P", Content: []byte("x"), ContentHash: "h", CreatedAt: 1, CreatedBy: "peer:alice"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st.GrantResource("prompt", "P", "bob", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeletePeer("alice", 3); err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetResource("prompt", "P")
	if r == nil || r.OwnerPeer != "" {
		t.Fatalf("resource should survive with operator owner: %+v", r)
	}

	// delete_resources=true removes owned resources and grants.
	st2 := newSubmissionStore(t)
	if err := st2.PutResource(&ResourceRevision{ID: "rr_a2", Kind: "prompt", Name: "Q", Content: []byte("x"), ContentHash: "h", CreatedAt: 1, CreatedBy: "peer:alice"}, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := st2.GrantResource("prompt", "Q", "bob", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.DeletePeerWithResourcePolicy("alice", 3, true); err != nil {
		t.Fatal(err)
	}
	if r, _ := st2.GetResource("prompt", "Q"); r != nil {
		t.Fatalf("resource should be deleted: %+v", r)
	}
	grants, _ := st2.ListResourceGrants("prompt", "Q")
	if len(grants) != 0 {
		t.Fatalf("grants should be deleted: %+v", grants)
	}
}

func TestStoreExpireAndCountSubmissions(t *testing.T) {
	st := newSubmissionStore(t)
	for i, id := range []string{"a", "b"} {
		if err := st.CreateResourceSubmission(&ResourceSubmission{
			ID: "rsub_" + id, Kind: "prompt", Name: id, Content: []byte("x"), ContentHash: "h",
			SubmittedBy: "alice", Status: "pending", CreatedAt: int64(i + 1), UpdatedAt: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.CountResourceSubmissions("alice", "pending", 0)
	if err != nil || n != 2 {
		t.Fatalf("count pending: %d %v", n, err)
	}
	if _, total, err := st.ListResourceSubmissions(ResourceSubmissionFilter{Q: "a"}); err != nil || total != 1 {
		t.Fatalf("list filter: total=%d err=%v", total, err)
	}
	expired, err := st.ExpirePendingSubmissions(2, 3)
	if err != nil || expired != 1 { // created_at < 2 → only the first
		t.Fatalf("expire: %d %v", expired, err)
	}
}

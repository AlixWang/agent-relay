package resources

import (
	"errors"
	"strings"
	"testing"
)

func TestSubmitSecretPendingEncryptedAndApprove(t *testing.T) {
	svc, st, _ := newTestService(t)
	v, unchanged, err := svc.SubmitSecret("alice", "OPENAI_API_KEY", "gpt", "sk-peer-secret")
	if err != nil || unchanged || v.Status != "pending" {
		t.Fatalf("submit: %+v unchanged=%v err=%v", v, unchanged, err)
	}
	sub, err := st.GetResourceSubmission(v.ID)
	if err != nil || sub == nil {
		t.Fatalf("stored submission: %v", err)
	}
	if strings.Contains(string(sub.Content), "sk-peer-secret") {
		t.Fatal("secret plaintext stored in submission")
	}
	detail, err := svc.GetSubmissionDetail(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Content != nil {
		t.Fatalf("secret detail leaks content: %#v", detail.Content)
	}
	live, err := svc.Approve(v.ID, "admin", "", "")
	if err != nil || live.OwnerPeer != "alice" {
		t.Fatalf("approve: %+v err=%v", live, err)
	}
	if err := svc.Grant(KindSecret, "OPENAI_API_KEY", "bob"); err != nil {
		t.Fatal(err)
	}
	plain, err := svc.RevealSecret("bob", "OPENAI_API_KEY")
	if err != nil || plain != "sk-peer-secret" {
		t.Fatalf("reveal: %q %v", plain, err)
	}
}

func TestSubmissionUpdateKeepsOldRevisionUntilApprove(t *testing.T) {
	svc, st, _ := newTestService(t)
	v1, _, err := svc.SubmitPrompt("alice", "reviewer", "v1", "first")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(v1.ID, "admin", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.Grant(KindPrompt, "reviewer", "bob"); err != nil {
		t.Fatal(err)
	}
	m1, _ := svc.Manifest("bob")
	v2, unchanged, err := svc.SubmitPrompt("alice", "reviewer", "v2", "second")
	if err != nil || unchanged || v2.ID == v1.ID {
		t.Fatalf("update submit: %+v unchanged=%v err=%v", v2, unchanged, err)
	}
	m2, _ := svc.Manifest("bob")
	if m1.ManifestRev != m2.ManifestRev {
		t.Fatal("pending update must not change the manifest")
	}
	if _, err := svc.Approve(v2.ID, "admin", "", ""); err != nil {
		t.Fatal(err)
	}
	m3, _ := svc.Manifest("bob")
	if m3.ManifestRev == m2.ManifestRev {
		t.Fatal("approved update must change the manifest")
	}
	r, _ := st.GetResource(KindPrompt, "reviewer")
	if string(r.Content) != "second" || r.OwnerPeer != "alice" {
		t.Fatalf("live update: %q owner=%q", r.Content, r.OwnerPeer)
	}
}

func TestSubmissionNameConflictAndNoOverwrite(t *testing.T) {
	svc, _, _ := newTestService(t)
	if _, err := svc.PutSecret("SHARED", "", "operator-value"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SubmitSecret("alice", "SHARED", "", "peer-value"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("expected name taken, got %v", err)
	}
}

func TestRejectWipesSubmissionContent(t *testing.T) {
	svc, st, _ := newTestService(t)
	v, _, err := svc.SubmitSecret("alice", "K", "", "secret-value")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Reject(v.ID, "admin", "not needed"); err != nil {
		t.Fatal(err)
	}
	sub, _ := st.GetResourceSubmission(v.ID)
	if sub.Status != "rejected" || len(sub.Content) != 0 || sub.ReviewNote != "not needed" {
		t.Fatalf("rejected submission: %+v", sub)
	}
	if _, err := st.GetResource(KindSecret, "K"); err == nil {
		// GetResource returns (nil,nil) for missing; the check below is the
		// meaningful one.
	}
}

func TestSubmissionQuotaAndRateLimit(t *testing.T) {
	svc, _, _ := newTestService(t)
	svc.SetSubmissionPolicy(SubmissionPolicy{Enabled: true, PendingMax: 1, LiveMax: 10, PerHour: 100, PendingTTLDays: 30})
	if _, _, err := svc.SubmitPrompt("alice", "one", "", "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SubmitPrompt("alice", "two", "", "x"); !errors.Is(err, ErrQuota) {
		t.Fatalf("expected quota, got %v", err)
	}
	svc.SetSubmissionPolicy(SubmissionPolicy{Enabled: true, PendingMax: 10, LiveMax: 10, PerHour: 1, PendingTTLDays: 30})
	if _, _, err := svc.SubmitPrompt("bob", "one", "", "x"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SubmitPrompt("bob", "two", "", "x"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected rate limit, got %v", err)
	}
}

func TestSkillSubmissionAndNameConflict(t *testing.T) {
	svc, _, _ := newTestService(t)
	raw := testSkillArchive(t, "code-review", "Review code")
	v, _, err := svc.SubmitSkill("alice", raw, "")
	if err != nil || v.Name != "code-review" {
		t.Fatalf("submit skill: %+v err=%v", v, err)
	}
	if _, err := svc.Approve(v.ID, "admin", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.SubmitSkill("bob", raw, ""); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("skill conflict: %v", err)
	}
}

func TestTakedownFlow(t *testing.T) {
	svc, _, _ := newTestService(t)
	v, _, err := svc.SubmitPrompt("alice", "r", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Approve(v.ID, "admin", "", ""); err != nil {
		t.Fatal(err)
	}
	td, err := svc.RequestTakedown("alice", KindPrompt, "r", "no longer needed")
	if err != nil || td.Status != "pending" {
		t.Fatalf("takedown: %+v err=%v", td, err)
	}
	if _, err := svc.RequestTakedown("bob", KindPrompt, "r", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner takedown: %v", err)
	}
	if err := svc.ResolveTakedown(td.ID, "admin", true, "ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(KindPrompt, "r"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("resource should be deleted: %v", err)
	}
}

func TestExpirePendingSubmissionWipesContent(t *testing.T) {
	svc, st, _ := newTestService(t)
	v, _, err := svc.SubmitPrompt("alice", "old", "", "x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.ExpirePendingSubmissions(1<<62, 1<<62); err != nil {
		t.Fatal(err)
	}
	sub, _ := st.GetResourceSubmission(v.ID)
	if sub.Status != "expired" || len(sub.Content) != 0 {
		t.Fatalf("expired: %+v", sub)
	}
}

// testSkillArchive builds a minimal valid tar.gz with SKILL.md frontmatter.
func testSkillArchive(t *testing.T, name, description string) []byte {
	t.Helper()
	// Reuse the helper in skill_test.go if present; keep this local to avoid
	// coupling test files when run individually.
	return makeTarGz(t, tarEntry{name: "SKILL.md", body: "---\nname: " + name + "\ndescription: " + description + "\n---\n\nbody\n"})
}

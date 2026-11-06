package resources

import (
	"reflect"
	"testing"
)

func TestRedactorMasksStoredSecrets(t *testing.T) {
	svc, _, _ := newTestService(t)
	r := svc.Redactor()
	if out, hits := r.Redact("nothing stored yet sk-live-abcdef12"); out != "nothing stored yet sk-live-abcdef12" || hits != nil {
		t.Fatalf("empty store: %q %v", out, hits)
	}
	_, _ = svc.PutSecret("OPENAI_API_KEY", "", "sk-live-abcdef12")
	_, _ = svc.PutSecret("GH", "", "ghp_0123456789")
	out, hits := r.Redact("key=sk-live-abcdef12 and again sk-live-abcdef12, gh ghp_0123456789")
	if out != "key=[secret:OPENAI_API_KEY] and again [secret:OPENAI_API_KEY], gh [secret:GH]" {
		t.Fatalf("masked: %q", out)
	}
	if !reflect.DeepEqual(hits, []string{"GH", "OPENAI_API_KEY"}) {
		t.Fatalf("hits: %v", hits)
	}
}

func TestRedactorCacheFollowsWrites(t *testing.T) {
	svc, _, _ := newTestService(t)
	r := svc.Redactor()
	_, _ = svc.PutSecret("K", "", "old-value-1234")
	if out, _ := r.Redact("old-value-1234"); out != "[secret:K]" {
		t.Fatalf("initial: %q", out)
	}
	// Rotation: the new value is masked, the retired one no longer is.
	_, _ = svc.PutSecret("K", "", "new-value-5678")
	if out, _ := r.Redact("old-value-1234 new-value-5678"); out != "old-value-1234 [secret:K]" {
		t.Fatalf("after rotation: %q", out)
	}
	// Deletion: nothing left to mask.
	_ = svc.Delete(KindSecret, "K")
	if out, hits := r.Redact("new-value-5678"); out != "new-value-5678" || hits != nil {
		t.Fatalf("after delete: %q %v", out, hits)
	}
}

func TestRedactorSkipsShortValuesAndPrefersLongest(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _ = svc.PutSecret("PIN", "", "1234")               // below MinRedactLen: never masked
	_, _ = svc.PutSecret("SHORT", "", "abcdefgh")         // contained in LONG
	_, _ = svc.PutSecret("LONG", "", "abcdefgh-ijklmnop") // must win over SHORT
	out, hits := svc.Redactor().Redact("pin 1234, token abcdefgh-ijklmnop, prefix abcdefgh")
	if out != "pin 1234, token [secret:LONG], prefix [secret:SHORT]" {
		t.Fatalf("masked: %q", out)
	}
	if !reflect.DeepEqual(hits, []string{"LONG", "SHORT"}) {
		t.Fatalf("hits: %v", hits)
	}
}

func TestRedactorNilSafe(t *testing.T) {
	var r *Redactor
	if out, hits := r.Redact("x"); out != "x" || hits != nil {
		t.Fatalf("nil redactor: %q %v", out, hits)
	}
}

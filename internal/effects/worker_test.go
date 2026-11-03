package effects

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

func openStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestWorkerExecutesAndRetries(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	if err := st.EnqueueEffect("test", "k1", "p1", now); err != nil {
		t.Fatal(err)
	}
	var calls int
	w := New(st)
	w.Register("test", func(ctx context.Context, e *store.Effect) error {
		calls++
		if calls == 1 {
			return errors.New("temporary")
		}
		return nil
	})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
	// Failed effect is still stored with one attempt and a future next_at.
	due, _ := st.DueEffects(now+300, 10)
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastError != "temporary" {
		t.Fatalf("failed effect: %+v", due)
	}
	time.Sleep(2100 * time.Millisecond)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("retry calls=%d", calls)
	}
	if due, _ := st.DueEffects(now+300, 10); len(due) != 0 {
		t.Fatalf("effect not completed: %+v", due)
	}
}

func TestEffectStatsCountsDueAndPerKind(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	if err := st.EnqueueEffect("webhook.offline", "due", "{}", now-10); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueEffect("webhook.offline", "future", "{}", now+300); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueEffect("other", "bad", "{}", now-1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.FailEffect(3, "boom", now-1); err != nil {
			t.Fatal(err)
		}
	}
	h, err := st.EffectStats(now)
	if err != nil {
		t.Fatal(err)
	}
	if h.Pending != 3 || h.Due != 2 || h.Failed != 1 {
		t.Fatalf("totals: %+v", h)
	}
	if h.Kinds["webhook.offline"].Pending != 2 || h.Kinds["webhook.offline"].Due != 1 {
		t.Fatalf("webhook kind: %+v", h.Kinds["webhook.offline"])
	}
	if h.Kinds["other"].LastError != "boom" || h.Kinds["other"].Failed != 1 {
		t.Fatalf("other kind: %+v", h.Kinds["other"])
	}
}

func TestUnknownHandlerIsRetriedAndVisible(t *testing.T) {
	st := openStore(t)
	now := time.Now().Unix()
	_ = st.EnqueueEffect("unknown", "k", "", now)
	w := New(st)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The row is not dropped; it is failed with a visible reason and a
	// backoff, so the next health read cannot claim the queue is idle.
	due, _ := st.DueEffects(now+400, 10)
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastError == "" {
		t.Fatalf("unknown effect not retried with error: %+v", due)
	}
	if w.LastError() == "" {
		t.Fatal("worker last error must surface an unhandled kind")
	}
	health, err := st.EffectStats(now + 400)
	if err != nil {
		t.Fatal(err)
	}
	if health.Pending != 1 || health.Due != 1 || health.Kinds["unknown"].LastError == "" {
		t.Fatalf("per-kind health: %+v", health)
	}
}

package stream

import "testing"

func TestSubscribeUnsubscribe(t *testing.T) {
	h := New(2, 10)
	a, err := h.Subscribe("alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Subscribe("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("alice"); err != ErrPeerLimit {
		t.Fatalf("peer cap: %v", err)
	}
	if n, total := h.Counts("alice"); n != 2 || total != 2 {
		t.Fatalf("counts: %d %d", n, total)
	}
	h.Unsubscribe(a)
	if n, _ := h.Counts("alice"); n != 1 {
		t.Fatalf("after unsub: %d", n)
	}
	h.Unsubscribe(b)
	// Idempotent.
	h.Unsubscribe(b)
	h.Unsubscribe(nil)
	if n, total := h.Counts("alice"); n != 0 || total != 0 {
		t.Fatalf("drained: %d %d", n, total)
	}
}

func TestTotalLimit(t *testing.T) {
	h := New(10, 2)
	if _, err := h.Subscribe("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Subscribe("c"); err != ErrTotalLimit {
		t.Fatalf("total cap: %v", err)
	}
}

func TestNotifyAllNonBlocking(t *testing.T) {
	h := New(3, 10)
	s, _ := h.Subscribe("alice")
	// Fill the buffer, then notify again: must not block.
	s.C <- struct{}{}
	h.NotifyAll()
	h.NotifyAll()
	select {
	case <-s.C:
	default:
		t.Fatal("expected at least one ping")
	}
	// At most one more pending (coalesced).
	n := 0
	for {
		select {
		case <-s.C:
			n++
		default:
			goto done
		}
	}
done:
	if n > 1 {
		t.Fatalf("pings coalesced, got %d", n)
	}
}

func TestLivePeers(t *testing.T) {
	var h *Hub
	if got := h.LivePeers(); got != nil {
		t.Fatalf("nil hub: %+v", got)
	}
	h = New(3, 10)
	if got := h.LivePeers(); len(got) != 0 {
		t.Fatalf("empty: %+v", got)
	}
	a, _ := h.Subscribe("alice")
	if got := h.LivePeers(); !got["alice"] {
		t.Fatalf("live: %+v", got)
	}
	h.Unsubscribe(a)
	if got := h.LivePeers(); got["alice"] {
		t.Fatalf("drained: %+v", got)
	}
}

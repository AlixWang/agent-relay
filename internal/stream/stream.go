// Package stream is the in-memory SSE fan-out for assistant delivery
// (DESIGN §4.4b). The database remains the source of truth: Publish only
// wakes subscribers, each of which re-queries VisibleTo for its own peer.
// Direct + broadcast, approval holds and ack filtering all stay in SQL, so
// the hub can never route a message the peer must not see.
//
// The hub is single-process memory by design. Losing it on restart is
// harmless: reconnecting clients resume with since/Last-Event-ID and the
// backlog replay covers the gap.
package stream

import (
	"errors"
	"sync"
)

// ErrPeerLimit is returned when one peer holds too many concurrent streams.
var ErrPeerLimit = errors.New("too many streams for peer")

// ErrTotalLimit is returned when the server holds too many streams overall.
var ErrTotalLimit = errors.New("too many streams")

// Sub is one held stream. C receives a wake-up ping (buffered, never
// blocks Publish). The channel is never closed: Unsubscribe drops the sub
// from the hub and the subscriber exits on request context; GC reclaims it.
type Sub struct {
	Peer string
	C    chan struct{}
}

// Hub tracks live streams.
type Hub struct {
	mu         sync.Mutex
	subs       map[*Sub]struct{}
	perPeer    map[string]int
	maxPerPeer int
	maxTotal   int
}

// New creates a Hub. Non-positive limits fall back to sane defaults.
func New(maxPerPeer, maxTotal int) *Hub {
	if maxPerPeer <= 0 {
		maxPerPeer = 3
	}
	if maxTotal <= 0 {
		maxTotal = 500
	}
	return &Hub{
		subs:       map[*Sub]struct{}{},
		perPeer:    map[string]int{},
		maxPerPeer: maxPerPeer,
		maxTotal:   maxTotal,
	}
}

// Subscribe registers a stream for peer. The caller must Unsubscribe on exit.
func (h *Hub) Subscribe(peer string) (*Sub, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.perPeer[peer] >= h.maxPerPeer {
		return nil, ErrPeerLimit
	}
	if len(h.subs) >= h.maxTotal {
		return nil, ErrTotalLimit
	}
	s := &Sub{Peer: peer, C: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	h.perPeer[peer]++
	return s, nil
}

// Unsubscribe removes a stream. Idempotent.
func (h *Hub) Unsubscribe(s *Sub) {
	if s == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[s]; !ok {
		return
	}
	delete(h.subs, s)
	if h.perPeer[s.Peer] > 0 {
		h.perPeer[s.Peer]--
	}
}

// NotifyAll wakes every subscriber. Slow consumers keep at most one pending
// ping: the backlog replay on wake-up covers anything missed, so Publish
// never blocks the sender.
func (h *Hub) NotifyAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs {
		select {
		case s.C <- struct{}{}:
		default:
		}
	}
}

// Counts returns (peer streams, total streams) for tests/metrics.
func (h *Hub) Counts(peer string) (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.perPeer[peer], len(h.subs)
}

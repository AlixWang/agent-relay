// Package effects runs the durable side-effect queue.
package effects

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Handler executes one effect. Returning an error schedules a retry.
type Handler func(ctx context.Context, e *store.Effect) error

// Worker polls due effects and dispatches them by kind.
type Worker struct {
	st       store.Store
	mu       sync.RWMutex
	handlers map[string]Handler
	lastErr  string
}

func New(st store.Store) *Worker {
	return &Worker{st: st, handlers: map[string]Handler{}}
}

func (w *Worker) Register(kind string, h Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[kind] = h
}

// Start runs a 2-second pass loop until ctx is cancelled.
func (w *Worker) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := w.RunOnce(ctx); err != nil {
					w.setErr(err)
					log.Printf("effects: %v", err)
				}
			}
		}
	}()
}

// RunOnce processes one batch of due effects. Each effect's failure is
// contained: it is retried with exponential backoff, never dropped. The worker
// records the first failure so /admin/health cannot look idle while a due row
// keeps failing or has no registered handler.
func (w *Worker) RunOnce(ctx context.Context) error {
	now := time.Now().Unix()
	due, err := w.st.DueEffects(now, 20)
	if err != nil {
		return fmt.Errorf("due effects: %w", err)
	}
	var firstErr error
	record := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range due {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		w.mu.RLock()
		h := w.handlers[e.Kind]
		w.mu.RUnlock()
		if h == nil {
			// Never silently skip: an unregistered kind is a real failure
			// (newer-binary row on an older build, or a registration bug) and
			// must surface in per-kind health while it keeps retrying.
			herr := fmt.Errorf("no handler registered for effect kind %q", e.Kind)
			if ferr := w.st.FailEffect(e.ID, herr.Error(), now+effectBackoff(e.Attempts)); ferr != nil {
				return ferr
			}
			record(herr)
			continue
		}
		if err := h(ctx, e); err != nil {
			if ferr := w.st.FailEffect(e.ID, err.Error(), now+effectBackoff(e.Attempts)); ferr != nil {
				return ferr
			}
			record(err)
			continue
		}
		if err := w.st.CompleteEffect(e.ID); err != nil {
			return err
		}
	}
	w.mu.Lock()
	if firstErr != nil {
		w.lastErr = firstErr.Error()
	} else {
		w.lastErr = ""
	}
	w.mu.Unlock()
	return nil
}

func effectBackoff(attempts int) int64 {
	delay := int64(1) << uint(minInt(attempts+1, 8)) // 2s..256s
	if delay > 300 {
		delay = 300
	}
	return delay
}

func (w *Worker) LastError() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.lastErr
}

func (w *Worker) setErr(err error) {
	w.mu.Lock()
	w.lastErr = err.Error()
	w.mu.Unlock()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

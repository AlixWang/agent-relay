package effects

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// RegisterWebhook handles the offline-presence webhook as a durable effect.
// Payload is {"url":"...","body":"..."} so the queue survives a restart and
// the URL is not re-read from config at execution time.
//
// The effect's kind and key are echoed as X-Agent-Relay-Effect-* headers so an
// at-least-once retry can be deduplicated by the receiver; the body itself also
// carries them when the producer followed the documented envelope.
func (w *Worker) RegisterWebhook() {
	w.Register("webhook.offline", func(ctx context.Context, e *store.Effect) error {
		var p struct {
			URL  string `json:"url"`
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			return fmt.Errorf("webhook payload: %w", err)
		}
		if p.URL == "" {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, "POST", p.URL, bytes.NewReader([]byte(p.Body)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Agent-Relay-Effect-Kind", e.Kind)
		req.Header.Set("X-Agent-Relay-Effect-Key", e.Key)
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook status %d", resp.StatusCode)
		}
		return nil
	})
}

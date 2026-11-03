package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/agentlog"
	"github.com/AlixWang/agent-relay/internal/store"
)

const agentLogBodyLimit = 256 << 10

// handleAgentLogs accepts a best-effort batch from an authenticated peer.
// It deliberately skips checkVersion: an old client must still be able to
// report why it cannot talk to the current server.
func (s *Server) handleAgentLogs(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	limit := int64(agentLogBodyLimit)
	if s.cfg.MaxBodyBytes > 0 && int64(s.cfg.MaxBodyBytes) < limit {
		limit = int64(s.cfg.MaxBodyBytes)
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	var batch agentlog.Batch
	dec := json.NewDecoder(r.Body)
	// Logs evolve independently of the message protocol. Ignore unknown
	// fields so a newer client can add diagnostics without a server upgrade.
	if err := dec.Decode(&batch); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge, "log batch too large")
			return
		}
		writeErr(w, http.StatusBadRequest, "bad json body")
		return
	}
	if batch.Client.Version == "" {
		batch.Client.Version = peer.ClientVersion
	}
	if batch.Client.Rev == "" {
		batch.Client.Rev = peer.ClientRev
	}
	res, err := s.agentLogs.Ingest(peer.ID, batch, time.Now().Unix())
	if err != nil {
		var ve *agentlog.ValidationError
		if errors.As(err, &ve) {
			writeErr(w, http.StatusBadRequest, ve.Reason)
			return
		}
		var re *agentlog.RateError
		if errors.As(err, &re) {
			retry := re.Retry
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false, "error": "log rate limited"})
			return
		}
		writeErr(w, http.StatusInternalServerError, "log ingest failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accepted": res.Accepted, "duplicates": res.Duplicates, "rejected": res.Rejected,
	})
}

func splitFilterValues(raw string) []string {
	out := []string{}
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func agentLogFilterFromQuery(r *http.Request) store.AgentLogFilter {
	q := r.URL.Query()
	now := time.Now().Unix()
	f := store.AgentLogFilter{
		PeerID: q.Get("peer"),
		Level:  splitFilterValues(q.Get("level")),
		Source: splitFilterValues(q.Get("source")),
		Event:  strings.TrimSpace(q.Get("event")),
		TaskID: q.Get("task_id"),
		Thread: q.Get("thread"),
		Q:      strings.TrimSpace(q.Get("q")),
	}
	if v, err := strconv.ParseInt(q.Get("since"), 10, 64); err == nil && v > 0 {
		f.Since = v
	} else {
		switch q.Get("range") {
		case "1h":
			f.Since = now - 3600
		case "7d":
			f.Since = now - 7*86400
		case "30d":
			f.Since = now - 30*86400
		default: // 24h
			f.Since = now - 86400
		}
	}
	if v, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil && v > 0 {
		f.Until = v
	}
	return f
}

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	p := parsePage(r)
	f := agentLogFilterFromQuery(r)
	entries, total, err := s.st.ListAgentLogsPage(f, p.Size, p.offset())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "logs failed")
		return
	}
	body := map[string]any{"ok": true, "entries": entries}
	if r.URL.Query().Get("facets") == "1" {
		sources, events, levels, err := s.st.AgentLogFacets(f)
		if err == nil {
			body["facets"] = map[string]any{"sources": sources, "events": events, "levels": levels}
		}
	}
	now := time.Now().Unix()
	if stats, err := s.st.AgentLogStats(now-86400, now); err == nil {
		body["stats"] = stats
	}
	writeJSON(w, http.StatusOK, withMeta(body, p, total))
}

func (s *Server) handleAdminLogsExport(w http.ResponseWriter, r *http.Request) {
	f := agentLogFilterFromQuery(r)
	limit := 10000
	entries, _, err := s.st.ListAgentLogsPage(f, limit, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "logs export failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="agent-logs.ndjson"`)
	enc := json.NewEncoder(w)
	for _, e := range entries {
		_ = enc.Encode(e)
	}
}

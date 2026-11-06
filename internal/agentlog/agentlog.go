// Package agentlog validates, sanitizes and rate-limits runtime diagnostics
// uploaded by assistants (DESIGN §4.11). It deliberately knows nothing about
// the message protocol: logs are best-effort operational data, never a reason
// to block task delivery.
package agentlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/AlixWang/agent-relay/internal/store"
)

const (
	MaxBatchEvents  = 100
	MaxMessageRunes = 500
	MaxFieldBytes   = 4096
	MaxTaskIDRunes  = 128
	MaxThreadRunes  = 128
)

var (
	ErrRateLimited   = errors.New("log rate limited")
	eventIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	sourcePattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	eventNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
)

// Event is one client-reported diagnostic.
type Event struct {
	ID      string         `json:"id"`
	Ts      int64          `json:"ts"`
	Level   string         `json:"level"`
	Source  string         `json:"source"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	TaskID  string         `json:"task_id"`
	Thread  string         `json:"thread"`
	Fields  map[string]any `json:"fields"`
}

// Batch is the POST /logs request body.
type Batch struct {
	Version int     `json:"version"`
	Client  Client  `json:"client"`
	Events  []Event `json:"events"`
}

type Client struct {
	Version string `json:"version"`
	Rev     string `json:"rev"`
}

// Result reports what happened to a batch.
type Result struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
}

// ValidationError is a structural problem: the caller should answer 400.
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return e.Reason }

// RateError tells the gateway to answer 429 with Retry-After.
type RateError struct{ Retry int }

func (e *RateError) Error() string { return ErrRateLimited.Error() }

// Ingestor owns the per-peer rate limiter and store writes.
type Ingestor struct {
	st      store.Store
	rate    int
	maxRows int
	mu      sync.Mutex
	hits    map[string][]int64

	redact     func(string) (string, []string)
	onRedacted func(peerID string, names []string)
}

func New(st store.Store, ratePerMinute, maxRowsPerPeer int) *Ingestor {
	if ratePerMinute <= 0 {
		ratePerMinute = 60
	}
	return &Ingestor{st: st, rate: ratePerMinute, maxRows: maxRowsPerPeer, hits: map[string][]int64{}}
}

// SetRedactor masks stored secret values in the free-text parts of an event
// (message, task/thread ids, field values; DESIGN §4.12, P4). onHit records
// an audit row per batch that had a hit. Nil keeps text untouched.
func (i *Ingestor) SetRedactor(fn func(string) (string, []string), onHit func(peerID string, names []string)) {
	i.redact, i.onRedacted = fn, onHit
}

// redactEvent masks secret values in place; it reports whether anything hit.
func (i *Ingestor) redactEvent(ev *Event) []string {
	if i.redact == nil {
		return nil
	}
	var hits []string
	str := func(s *string) {
		if *s == "" {
			return
		}
		if masked, h := i.redact(*s); masked != *s {
			*s, hits = masked, append(hits, h...)
		}
	}
	str(&ev.Message)
	str(&ev.TaskID)
	str(&ev.Thread)
	for k, v := range ev.Fields {
		if s, ok := v.(string); ok && s != "" {
			if masked, h := i.redact(s); masked != s {
				ev.Fields[k], hits = masked, append(hits, h...)
			}
		}
	}
	return hits
}

// Ingest validates and stores a batch. Invalid individual events are counted
// in Result.Rejected and skipped; structural errors return *ValidationError.
func (i *Ingestor) Ingest(peerID string, b Batch, now int64) (*Result, error) {
	if len(b.Events) == 0 {
		return nil, &ValidationError{Reason: "events is required"}
	}
	if len(b.Events) > MaxBatchEvents {
		return nil, &ValidationError{Reason: fmt.Sprintf("at most %d events per batch", MaxBatchEvents)}
	}
	if b.Version != 0 && b.Version != 1 {
		return nil, &ValidationError{Reason: "unsupported log schema version"}
	}
	// Rate limit counts every submitted event, including malformed ones: a
	// bad client cannot use validation failures to bypass the cap.
	if retry, ok := i.allow(peerID, len(b.Events), now); !ok {
		return nil, &RateError{Retry: retry}
	}
	result := &Result{}
	var redacted []string
	batch := make([]*store.AgentLog, 0, len(b.Events))
	for _, in := range b.Events {
		redacted = append(redacted, i.redactEvent(&in)...)
		ev, err := normalize(in, now)
		if err != nil {
			result.Rejected++
			continue
		}
		ev.PeerID = peerID
		ev.ReceivedAt = now
		ev.ClientVersion = truncate(b.Client.Version, 64)
		ev.ClientRev = truncate(b.Client.Rev, 64)
		batch = append(batch, ev)
	}
	if len(batch) == 0 {
		return result, nil
	}
	if len(redacted) > 0 && i.onRedacted != nil {
		uniq := map[string]bool{}
		var names []string
		for _, n := range redacted {
			if !uniq[n] {
				uniq[n] = true
				names = append(names, n)
			}
		}
		sort.Strings(names)
		i.onRedacted(peerID, names)
	}
	accepted, duplicates, err := i.st.InsertAgentLogs(batch)
	if err != nil {
		return nil, err
	}
	result.Accepted = accepted
	result.Duplicates = duplicates
	if i.maxRows > 0 {
		_, _ = i.st.TrimAgentLogs(peerID, i.maxRows)
	}
	return result, nil
}

func (i *Ingestor) allow(peerID string, n int, now int64) (int, bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	window := i.hits[peerID]
	kept := window[:0]
	for _, t := range window {
		if now-t < 60 {
			kept = append(kept, t)
		}
	}
	if len(kept)+n > i.rate {
		retry := 1
		if len(kept) > 0 {
			retry = int(60 - (now - kept[0]))
			if retry < 1 {
				retry = 1
			}
		}
		i.hits[peerID] = kept
		return retry, false
	}
	for j := 0; j < n; j++ {
		kept = append(kept, now)
	}
	i.hits[peerID] = kept
	return 0, true
}

func normalize(in Event, now int64) (*store.AgentLog, error) {
	id := strings.TrimSpace(in.ID)
	if !eventIDPattern.MatchString(id) {
		return nil, fmt.Errorf("bad event id")
	}
	level := strings.ToLower(strings.TrimSpace(in.Level))
	if level == "warning" {
		level = "warn"
	}
	switch level {
	case "debug", "info", "warn", "error":
	default:
		return nil, fmt.Errorf("bad level")
	}
	source := strings.ToLower(strings.TrimSpace(in.Source))
	if !sourcePattern.MatchString(source) {
		return nil, fmt.Errorf("bad source")
	}
	name := strings.TrimSpace(in.Event)
	if !eventNamePattern.MatchString(name) {
		return nil, fmt.Errorf("bad event name")
	}
	ts := in.Ts
	if ts == 0 || ts > now+300 || ts < now-7*86400 {
		ts = now
	}
	fields, err := sanitizeFields(in.Fields)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(fields)
	if len(raw) > MaxFieldBytes {
		return nil, fmt.Errorf("fields too large")
	}
	return &store.AgentLog{
		EventID: id,
		Ts:      ts,
		Level:   level,
		Source:  source,
		Event:   name,
		Message: truncate(in.Message, MaxMessageRunes),
		TaskID:  truncate(strings.TrimSpace(in.TaskID), MaxTaskIDRunes),
		Thread:  truncate(strings.TrimSpace(in.Thread), MaxThreadRunes),
		Fields:  string(raw),
	}, nil
}

var secretKeys = []string{
	"token", "authorization", "cookie", "secret", "password", "passwd",
	"credential", "api_key", "apikey", "hatch_sess", "hatch_vml", "set-cookie",
}

func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range secretKeys {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

func sanitizeFields(in map[string]any) (map[string]any, error) {
	if in == nil {
		return map[string]any{}, nil
	}
	out := map[string]any{}
	for k, v := range in {
		if isSecretKey(k) {
			continue
		}
		clean, ok := sanitizeValue(v, 0)
		if ok {
			out[k] = clean
		}
	}
	return out, nil
}

func sanitizeValue(v any, depth int) (any, bool) {
	if depth > 3 {
		return "[truncated]", true
	}
	switch x := v.(type) {
	case nil, bool, float64, int, int64, json.Number:
		return x, true
	case string:
		return truncate(x, 256), true
	case []any:
		if len(x) > 20 {
			x = x[:20]
		}
		out := make([]any, 0, len(x))
		for _, item := range x {
			if c, ok := sanitizeValue(item, depth+1); ok {
				out = append(out, c)
			}
		}
		return out, true
	case map[string]any:
		out := map[string]any{}
		for k, item := range x {
			if isSecretKey(k) {
				continue
			}
			if c, ok := sanitizeValue(item, depth+1); ok {
				out[k] = c
			}
		}
		return out, true
	default:
		return fmt.Sprintf("%v", x), true
	}
}

func truncate(s string, max int) string {
	if max <= 0 || utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}

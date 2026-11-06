package resources

import (
	"sort"
	"strings"
	"sync"
)

// MinRedactLen is the shortest secret value that gets masked. Shorter values
// ("1234", "dev") would turn ordinary words into [secret:X] noise; such a
// value is not a meaningful credential anyway.
const MinRedactLen = 8

// Redactor masks stored secret values inside free text (DESIGN §4.12, P4):
// message payloads and agent logs pass through it before they are stored, so
// a secret an assistant pastes back never lands in the database or the
// console. Deterministic, no LLM: plain substring replacement over the
// decrypted values, longest first so a value containing another wins.
type Redactor struct {
	svc *Service

	mu    sync.RWMutex
	cache []secretValue // nil = stale
}

type secretValue struct{ name, value string }

// Redactor returns the service's redactor; the cache is invalidated on every
// secret write or delete.
func (s *Service) Redactor() *Redactor {
	s.redactorOnce.Do(func() { s.redactor = &Redactor{svc: s} })
	return s.redactor
}

func (r *Redactor) invalidate() {
	r.mu.Lock()
	r.cache = nil
	r.mu.Unlock()
}

func (r *Redactor) values() []secretValue {
	r.mu.RLock()
	c := r.cache
	r.mu.RUnlock()
	if c != nil {
		return c
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cache != nil {
		return r.cache
	}
	rs, err := r.svc.st.ListResources(KindSecret)
	if err != nil {
		return nil // do not cache a failure; try again next call
	}
	out := []secretValue{}
	for _, res := range rs {
		plain, err := open(r.svc.key, res.Content)
		if err != nil || len(plain) < MinRedactLen {
			continue
		}
		out = append(out, secretValue{res.Name, string(plain)})
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].value) > len(out[j].value) })
	r.cache = out
	return out
}

// Redact replaces every stored secret value in text with [secret:NAME] and
// reports which secrets it found (sorted, de-duplicated).
func (r *Redactor) Redact(text string) (string, []string) {
	if r == nil || text == "" {
		return text, nil
	}
	var hits []string
	for _, sv := range r.values() {
		if strings.Contains(text, sv.value) {
			text = strings.ReplaceAll(text, sv.value, "[secret:"+sv.name+"]")
			hits = append(hits, sv.name)
		}
	}
	sort.Strings(hits)
	return text, hits
}

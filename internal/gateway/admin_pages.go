package gateway

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Console pagination (DESIGN §9). Every admin list accepts
// ?page=<1-based>&page_size=<n> and answers with total/page/page_size next
// to its legacy key (peers/tokens/threads/items/entries), so existing
// scripts that ignore the new fields keep working.

const (
	defaultPageSize = 20
	maxPageSize     = 200
)

type pageReq struct {
	Page, Size int
	Paged      bool // caller sent page or page_size explicitly
}

func parsePage(r *http.Request) pageReq {
	q := r.URL.Query()
	p := pageReq{Page: 1, Size: defaultPageSize}
	if v, err := strconv.Atoi(q.Get("page")); err == nil && v > 0 {
		p.Page, p.Paged = v, true
	}
	if v, err := strconv.Atoi(q.Get("page_size")); err == nil && v > 0 {
		p.Size, p.Paged = v, true
	}
	if p.Size > maxPageSize {
		p.Size = maxPageSize
	}
	return p
}

func (p pageReq) offset() int { return (p.Page - 1) * p.Size }

func (p pageReq) meta(total int) map[string]any {
	return map[string]any{"total": total, "page": p.Page, "page_size": p.Size}
}

// slicePage applies in-memory paging to small, already-loaded lists
// (peers, tokens). Unpaged callers get the full list (legacy behavior).
func slicePage[T any](all []T, p pageReq) []T {
	if !p.Paged {
		return all
	}
	start := p.offset()
	if start >= len(all) {
		return []T{}
	}
	end := start + p.Size
	if end > len(all) {
		end = len(all)
	}
	return all[start:end]
}

func withMeta(body map[string]any, p pageReq, total int) map[string]any {
	for k, v := range p.meta(total) {
		body[k] = v
	}
	return body
}

// filterPeers applies console filters: q (id/display/profile substring),
// status, type, online=1|0, transport=sse|poll.
func filterPeers(views []*presence.PeerView, r *http.Request) []*presence.PeerView {
	q := r.URL.Query()
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	status, typ, online, transport := q.Get("status"), q.Get("type"), q.Get("online"), q.Get("transport")
	// status accepts a comma list so the console can group lifecycle states
	// (e.g. status=pending,verifying) behind one chip.
	var states []string
	for _, s := range strings.Split(status, ",") {
		if s = strings.TrimSpace(s); s != "" {
			states = append(states, s)
		}
	}
	if needle == "" && len(states) == 0 && typ == "" && online == "" && transport == "" {
		return views
	}
	out := make([]*presence.PeerView, 0, len(views))
	for _, v := range views {
		if needle != "" && !strings.Contains(strings.ToLower(v.ID+"\n"+v.DisplayName+"\n"+v.Profile), needle) {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, v.Status) {
			continue
		}
		if typ != "" && v.AgentType != typ {
			continue
		}
		if online == "1" && !v.Online || online == "0" && v.Online {
			continue
		}
		if transport != "" {
			t := v.Transport
			if t == "" {
				t = "poll"
			}
			if t != transport {
				continue
			}
		}
		out = append(out, v)
	}
	return out
}

// filterTokens: peer (exact), state=active|revoked, q (label/peer substring).
func filterTokens(toks []*store.TokenRow, r *http.Request) []*store.TokenRow {
	q := r.URL.Query()
	peer, state := q.Get("peer"), q.Get("state")
	needle := strings.ToLower(strings.TrimSpace(q.Get("q")))
	if peer == "" && state == "" && needle == "" {
		return toks
	}
	out := make([]*store.TokenRow, 0, len(toks))
	for _, t := range toks {
		if peer != "" && t.PeerID != peer {
			continue
		}
		if state == "active" && t.RevokedAt != 0 || state == "revoked" && t.RevokedAt == 0 {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(t.PeerID+"\n"+t.Label+"\n"+t.LastIP), needle) {
			continue
		}
		out = append(out, t)
	}
	return out
}

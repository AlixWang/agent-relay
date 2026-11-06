// Package presence tracks heartbeats and emits offline transitions (DESIGN §4.5).
package presence

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync"

	"github.com/AlixWang/agent-relay/internal/store"
)

// PeerView is the wire shape for GET /peers.
type PeerView struct {
	ID           string `json:"id"`
	DisplayName  string `json:"display_name"`
	AgentType    string `json:"agent_type"`
	Status       string `json:"status"`
	LastSeen     int64  `json:"last_seen"`
	Online       bool   `json:"online"`
	ProtocolVer  int    `json:"protocol_version"`
	Capabilities any    `json:"capabilities,omitempty"`
	OfflineSecs  int64  `json:"offline_secs,omitempty"`
	// Prompt distribution (§8.6): which instruction revision the peer runs
	// and when it confirmed. UI shows laggards; server nudges via heartbeat.
	PromptVersion   int   `json:"prompt_version"`
	PromptUpdatedAt int64 `json:"prompt_updated_at,omitempty"`
	// Assistant profile (§8.7): self-reported capabilities + usual tasks
	// for routing unassigned work. Shown in UI, searchable via /peers.
	Profile          string `json:"profile,omitempty"`
	ProfileUpdatedAt int64  `json:"profile_updated_at,omitempty"`
	// Receiver client version (§8.9): which relay-tail build the peer runs
	// ("" = not reporting, e.g. shell scripts). Shown in UI; the server
	// nudges via heartbeat when a newer Release exists.
	ClientVersion   string `json:"client_version,omitempty"`
	ClientUpdatedAt int64  `json:"client_updated_at,omitempty"`
	// ClientRev is the receiver source revision (§8.9) — the value that
	// actually decides whether the peer needs a new binary. A version-tag
	// mismatch with an equal rev is not an update.
	ClientRev string `json:"client_rev,omitempty"`
	// Memory reconciliation (§8.6): the assistant's report of which
	// conflicting relay rules it purged from memory, for which revision.
	MemoryNote         string `json:"memory_note,omitempty"`
	MemoryVersion      int    `json:"memory_version"`
	MemoryReconciledAt int64  `json:"memory_reconciled_at,omitempty"`
	// AdminNote is the operator-only member remark (e.g. login email).
	// Served only on GET /admin/peers; the agent-facing GET /peers
	// strips it so assistants never see it.
	AdminNote string `json:"admin_note,omitempty"`
	// Resource hub receipt (§4.12): the manifest digest the assistant last
	// applied. ResourcesPending is filled by the gateway on GET /admin/peers
	// (it needs the hub to compute the current digest).
	ResourcesRev       string `json:"resources_rev,omitempty"`
	ResourcesUpdatedAt int64  `json:"resources_updated_at,omitempty"`
	ResourcesPending   bool   `json:"resources_pending,omitempty"`
	// ResourceSubmitEnabled is the operator-controlled per-peer switch for
	// self-service resource registration (ADR 0006). Admin-only.
	ResourceSubmitEnabled bool `json:"resource_submit_enabled,omitempty"`
	// Transport (§4.4b): how the peer currently receives messages.
	// "sse" = holding a live /messages/stream; "poll" = short-polling
	// (or offline/unknown). Computed from the stream hub, not stored.
	Transport string `json:"transport,omitempty"`
	// Muse is the operator-only Muse authorization summary (§4.9). Filled by
	// the gateway on GET /admin/peers; nil on the agent-facing GET /peers.
	Muse *PeerMuseView `json:"muse,omitempty"`
}

// PeerMuseView is the small member-table summary of a Muse authorization.
// It carries no cookie material.
type PeerMuseView struct {
	AccountID       string `json:"account_id"`
	AuthState       string `json:"auth_state"`
	VMState         string `json:"vm_state,omitempty"`
	ExpiresAt       int64  `json:"expires_at,omitempty"`
	LastKeepaliveAt int64  `json:"last_keepalive_at,omitempty"`
	LastWakeAt      int64  `json:"last_wake_at,omitempty"`
	LastAttemptOK   bool   `json:"last_attempt_ok"`
	LastError       string `json:"last_error,omitempty"`
	Enabled         bool   `json:"enabled"`
	AutoWake        bool   `json:"auto_wake"`
}

// Service tracks last-seen and fires offline transitions.
type Service struct {
	st            store.Store
	onlineTimeout int64
	webhookURL    string
	// OnOffline is called after an online→offline transition is audited and
	// before the webhook. It must not block: the Muse integration spawns its
	// own goroutine. Nil disables the hook.
	OnOffline func(peerID string, offlineSecs int64)

	mu        sync.Mutex
	wasOnline map[string]bool
}

// New creates a presence Service.
func New(st store.Store, onlineTimeoutSecs int64, webhookURL string) *Service {
	if onlineTimeoutSecs <= 0 {
		onlineTimeoutSecs = 300
	}
	return &Service{
		st:            st,
		onlineTimeout: onlineTimeoutSecs,
		webhookURL:    webhookURL,
		wasOnline:     map[string]bool{},
	}
}

// Beat records a heartbeat and refreshes reported version/capabilities.
// The wasOnline baseline suppresses a spurious offline event on first sight.
// promptVersion is the peer's confirmed worker-instruction revision (§8.6);
// <=0 means "not reporting" (old client) and leaves the stored value alone.
// profile is the peer's self-reported intro (§8.7); empty keeps the stored
// value, non-empty overwrites with now as profile_updated_at.
// clientVersion is the receiver client build (§8.9); empty keeps the stored
// value (shell scripts don't report), non-empty overwrites.
// ClientRev records the receiver source revision reported in a heartbeat
// (DESIGN §8.9). Kept separate from Beat so the presence API stays stable;
// empty means "not reporting" and never clobbers a stored value.
func (s *Service) BeatClientRev(peerID, clientRev string, now int64) error {
	if clientRev == "" {
		return nil
	}
	return s.st.UpdatePeerClientRev(peerID, clientRev, now)
}

// BeatMemory records a memory-reconciliation report (§8.6). Empty note
// means "not reporting" and never clobbers a stored value.
func (s *Service) BeatMemory(peerID, note string, version int, now int64) error {
	if note == "" || version <= 0 {
		return nil
	}
	return s.st.UpdatePeerMemory(peerID, note, version, now)
}

func (s *Service) Beat(peerID string, protocolVer int, capabilities string, promptVersion int, profile string, clientVersion string, now int64) error {
	if err := s.st.TouchPeer(peerID, now); err != nil {
		return err
	}
	if protocolVer > 0 || capabilities != "" {
		peer, err := s.st.GetPeer(peerID)
		if err != nil || peer == nil {
			return err
		}
		if protocolVer <= 0 {
			protocolVer = peer.ProtocolVer
		}
		if capabilities == "" {
			capabilities = peer.Capabilities
		}
		_ = s.st.UpdatePeerCapabilities(peerID, protocolVer, capabilities)
	}
	if promptVersion > 0 {
		_ = s.st.UpdatePeerPrompt(peerID, promptVersion, now)
	}
	if profile != "" {
		_ = s.st.UpdatePeerProfile(peerID, profile, now)
	}
	if clientVersion != "" {
		_ = s.st.UpdatePeerClient(peerID, clientVersion, now)
	}
	// First sighting: mark online baseline so we don't fire a spurious
	// offline event before the first timeout window passes.
	s.mu.Lock()
	if _, seen := s.wasOnline[peerID]; !seen {
		s.wasOnline[peerID] = true
	}
	s.mu.Unlock()
	return nil
}

// List returns all peers with online computed.
// live, when non-nil, marks peers holding an SSE stream: their transport
// is reported as "sse", everyone else as "poll" (§4.4b). The map comes
// from the stream hub (LivePeers); nil means "hub absent, all poll".
func (s *Service) List(now int64, live map[string]bool) ([]*PeerView, error) {
	peers, err := s.st.ListPeers()
	if err != nil {
		return nil, err
	}
	out := make([]*PeerView, 0, len(peers))
	for _, p := range peers {
		online := now-p.LastSeen < s.onlineTimeout && p.LastSeen != 0
		v := &PeerView{
			ID: p.ID, DisplayName: p.DisplayName, AgentType: p.AgentType,
			Status: p.Status, LastSeen: p.LastSeen, Online: online,
			ProtocolVer:   p.ProtocolVer,
			PromptVersion: p.PromptVersion, PromptUpdatedAt: p.PromptUpdatedAt,
			Profile: p.Profile, ProfileUpdatedAt: p.ProfileUpdatedAt,
			ClientVersion: p.ClientVersion, ClientUpdatedAt: p.ClientUpdatedAt,
			ClientRev:  p.ClientRev,
			MemoryNote: p.MemoryNote, MemoryVersion: p.MemoryVersion, MemoryReconciledAt: p.MemoryReconciledAt,
			AdminNote:    p.AdminNote,
			ResourcesRev: p.ResourcesRev, ResourcesUpdatedAt: p.ResourcesUpdatedAt,
			ResourceSubmitEnabled: p.ResourceSubmitEnabled,
		}
		var caps any
		if err := json.Unmarshal([]byte(orEmptyJSON(p.Capabilities)), &caps); err == nil {
			v.Capabilities = caps
		}
		if live != nil && live[p.ID] {
			v.Transport = "sse"
		} else {
			v.Transport = "poll"
		}
		if !online && p.LastSeen != 0 {
			v.OfflineSecs = now - p.LastSeen
		}
		out = append(out, v)
	}
	return out, nil
}

func orEmptyJSON(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// Sweep checks for online→offline transitions, audits them and optionally
// enqueues the webhook. Run every 30s from main.
//
// The audit row and the durable effect are committed in one transaction. The
// in-memory baseline is only flipped after that commit succeeds, so a database
// failure is retried on the next sweep instead of losing the alert.
func (s *Service) Sweep(now int64) {
	peers, err := s.st.ListPeers()
	if err != nil {
		return
	}
	for _, p := range peers {
		online := now-p.LastSeen < s.onlineTimeout && p.LastSeen != 0
		s.mu.Lock()
		prev, tracked := s.wasOnline[p.ID]
		if !tracked {
			// Baseline without event: presence starts reporting from here.
			s.wasOnline[p.ID] = online
			s.mu.Unlock()
			continue
		}
		if !prev || online {
			s.wasOnline[p.ID] = online
			s.mu.Unlock()
			continue
		}
		s.mu.Unlock()

		offlineSecs := now - p.LastSeen
		kind, key, payload := s.webhookEffect(p.ID, offlineSecs, now)
		if err := s.st.RecordAuditAndEffect("system", "peer.offline",
			"peer="+p.ID+formatSecs(offlineSecs), kind, key, payload, now); err != nil {
			log.Printf("presence: offline transition for %s not persisted: %v", p.ID, err)
			continue
		}
		s.mu.Lock()
		s.wasOnline[p.ID] = false
		s.mu.Unlock()
		if s.OnOffline != nil {
			s.OnOffline(p.ID, offlineSecs)
		}
	}
}

func formatSecs(s int64) string {
	return " offline_secs=" + strconv.FormatInt(s, 10)
}

// webhookEffect builds the durable-effect triple for an offline transition.
// The key is stable across retries; the body carries it too so a receiver can
// deduplicate an at-least-once POST without reading custom headers.
func (s *Service) webhookEffect(peerID string, offlineSecs, now int64) (kind, key, payload string) {
	if s.webhookURL == "" {
		return "", "", ""
	}
	kind = "webhook.offline"
	key = fmt.Sprintf("offline:%s:%d", peerID, now)
	body, _ := json.Marshal(map[string]any{
		"kind": kind, "key": key,
		"peer_id": peerID, "offline_secs": offlineSecs, "ts": now,
	})
	envelope, _ := json.Marshal(map[string]string{
		"kind": kind, "key": key, "url": s.webhookURL, "body": string(body),
	})
	return kind, key, string(envelope)
}

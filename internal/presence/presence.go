// Package presence tracks heartbeats and emits offline transitions (DESIGN §4.5).
package presence

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

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
}

// Service tracks last-seen and fires offline transitions.
type Service struct {
	st            store.Store
	onlineTimeout int64
	webhookURL    string

	mu        sync.Mutex
	wasOnline map[string]bool
	client    *http.Client
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
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

// Beat records a heartbeat and refreshes reported version/capabilities.
// The wasOnline baseline suppresses a spurious offline event on first sight.
// promptVersion is the peer's confirmed worker-instruction revision (§8.6);
// <=0 means "not reporting" (old client) and leaves the stored value alone.
// profile is the peer's self-reported intro (§8.7); empty keeps the stored
// value, non-empty overwrites with now as profile_updated_at.
func (s *Service) Beat(peerID string, protocolVer int, capabilities string, promptVersion int, profile string, now int64) error {
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
func (s *Service) List(now int64) ([]*PeerView, error) {
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
		}
		var caps any
		if err := json.Unmarshal([]byte(orEmptyJSON(p.Capabilities)), &caps); err == nil {
			v.Capabilities = caps
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
// POSTs the webhook. Run every 30s from main.
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
		s.wasOnline[p.ID] = online
		s.mu.Unlock()
		if prev && !online {
			offlineSecs := now - p.LastSeen
			_ = s.st.AppendAudit("system", "peer.offline",
				"peer="+p.ID+formatSecs(offlineSecs), now)
			s.fireWebhook(p.ID, offlineSecs, now)
		}
	}
}

func formatSecs(s int64) string {
	return " offline_secs=" + strconv.FormatInt(s, 10)
}

func (s *Service) fireWebhook(peerID string, offlineSecs, now int64) {
	if s.webhookURL == "" {
		return
	}
	body, _ := json.Marshal(map[string]any{
		"peer_id": peerID, "offline_secs": offlineSecs, "ts": now,
	})
	req, err := http.NewRequest("POST", s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

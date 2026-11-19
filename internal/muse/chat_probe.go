package muse

// chat_probe.go is the explicitly labelled, hidden approval probe used to
// answer the P0-P2 questions in the design report §4.5 before any approval
// card UI exists. It reuses the live chat job's connection and lease; it never
// opens a second connection, never audits payloads and never decides anything
// on its own. Every call is one operator-requested step on service 1
// (sentinel).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	chatProbeService    = 1 // sentinel
	chatProbeTimeout    = 20 * time.Second
	chatProbeApprovals  = "/approvals"
	chatProbeDecidePath = "/approvals/%s/decide"

	ChatProbeVariantWithID       = "with_id"
	ChatProbeVariantDecisionOnly = "decision_only"
	ChatProbeVariantEmpty        = "empty"

	ChatProbeDecisionDeny      = "deny"
	ChatProbeDecisionAllowOnce = "allow_once"
)

var chatApprovalIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,256}$`)

// ChatProbeResult is the raw upstream answer. Body is shown to the operator in
// the probe environment and is never written to audit or a server log.
type ChatProbeResult struct {
	Service        int
	Status         int
	Body           []byte
	RequestVariant string
	Decision       string
}

type chatProbeSession interface {
	serviceCall(ctx context.Context, service int, verb, path string, body []byte) (*appResponse, error)
}

// ProbeApprovals runs P0/P1: GET /approvals on sentinel over the job's own
// connection. A 2xx proves the service is reachable; the body lists pending
// approvals and their full fields (including decision_options and scope).
func (s *Service) ProbeApprovals(ctx context.Context, chatID string) (*ChatProbeResult, error) {
	sess, err := s.probeSession(chatID)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, chatProbeTimeout)
	defer cancel()
	resp, err := sess.serviceCall(cctx, chatProbeService, http.MethodGet, chatProbeApprovals, nil)
	if err != nil {
		return nil, err
	}
	return &ChatProbeResult{Service: chatProbeService, Status: resp.Status, Body: resp.Body}, nil
}

// ProbeDecide runs one P2 step. decision must be deny or allow_once;
// allow_always is deliberately unreachable. variant selects exactly one of the
// three documented request-body candidates:
//
//	with_id       {"approval_id":"...","decision":"..."}
//	decision_only {"decision":"..."}
//	empty         {}
//
// A 2xx only means the request was accepted. The caller must re-run
// ProbeApprovals (and watch history) to see whether the decision took effect.
func (s *Service) ProbeDecide(ctx context.Context, chatID, approvalID, decision, variant string) (*ChatProbeResult, error) {
	approvalID = strings.TrimSpace(approvalID)
	if !chatApprovalIDPattern.MatchString(approvalID) {
		return nil, admissionErr(400, "invalid_approval_id", 0)
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	switch decision {
	case ChatProbeDecisionDeny, ChatProbeDecisionAllowOnce:
	case "allow_always":
		return nil, admissionErr(400, "probe_allow_always_forbidden", 0)
	default:
		return nil, admissionErr(400, "invalid_decision", 0)
	}

	var body []byte
	switch variant {
	case ChatProbeVariantWithID:
		body, _ = json.Marshal(map[string]string{"approval_id": approvalID, "decision": decision})
	case ChatProbeVariantDecisionOnly:
		body, _ = json.Marshal(map[string]string{"decision": decision})
	case ChatProbeVariantEmpty:
		body = nil
	default:
		return nil, admissionErr(400, "invalid_probe_variant", 0)
	}

	sess, err := s.probeSession(chatID)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, chatProbeTimeout)
	defer cancel()
	path := fmt.Sprintf(chatProbeDecidePath, url.PathEscape(approvalID))
	resp, err := sess.serviceCall(cctx, chatProbeService, http.MethodPost, path, body)
	if err != nil {
		return nil, err
	}
	return &ChatProbeResult{
		Service: chatProbeService, Status: resp.Status, Body: resp.Body,
		RequestVariant: variant, Decision: decision,
	}, nil
}

// probeSession reuses the job's existing live session. A terminal job has
// already released its session (nil), so the probe returns ErrChatNotActive
// rather than dialing a new connection.
func (s *Service) probeSession(chatID string) (chatProbeSession, error) {
	if s == nil {
		return nil, admissionErr(503, "muse_chat_disabled", 0)
	}
	s.chatMu.Lock()
	job := s.chatJobs[strings.TrimSpace(chatID)]
	s.chatMu.Unlock()
	if job == nil {
		return nil, ErrChatNotFound
	}
	job.mu.Lock()
	sess := job.session
	terminal := isChatTerminal(job.Status)
	job.mu.Unlock()
	if terminal || sess == nil {
		return nil, ErrChatNotActive
	}
	probe, ok := sess.(chatProbeSession)
	if !ok {
		return nil, ErrChatNotActive
	}
	return probe, nil
}

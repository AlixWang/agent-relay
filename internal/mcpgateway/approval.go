package mcpgateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// ApprovalManager owns the frozen-params hash and the CAS state machine
// (design v5 §5). The store does the CAS; this type maps row counts to the
// explicit client errors and never mutates a terminal row.
type ApprovalManager struct {
	st  store.Store
	cfg Config
	now func() time.Time
}

func NewApprovalManager(st store.Store, cfg Config) *ApprovalManager {
	return &ApprovalManager{st: st, cfg: cfg, now: time.Now}
}

func (m *ApprovalManager) SetClock(now func() time.Time) {
	if now != nil {
		m.now = now
	}
}

// FreezeParams canonicalizes tool+params into the stored frozen form:
//
//	params_json = canonical JSON object without control fields
//	params_hash = SHA-256 hex of {"tool":<tool>,"params":<canonical>}
//
// Control fields (dry_run, approval_id) are excluded because the execute call
// must be able to add approval_id without changing the frozen request.
func FreezeParams(tool string, raw json.RawMessage) (canonical []byte, hash string, err error) {
	params, err := stripControlFields(raw)
	if err != nil {
		return nil, "", fmt.Errorf("invalid params: %w", err)
	}
	canonical, err = canonicalJSON(params)
	if err != nil {
		return nil, "", fmt.Errorf("invalid params: %w", err)
	}
	wrapper, err := json.Marshal(map[string]any{
		"tool":   tool,
		"params": json.RawMessage(canonical),
	})
	if err != nil {
		return nil, "", fmt.Errorf("freeze params: %w", err)
	}
	sum := sha256.Sum256(wrapper)
	return canonical, hex.EncodeToString(sum[:]), nil
}

func stripControlFields(raw json.RawMessage) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("trailing data")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("params must be a JSON object")
	}
	delete(obj, "dry_run")
	delete(obj, "approval_id")
	return obj, nil
}

// canonicalJSON sorts keys (encoding/json does this for map[string]any) and
// normalizes numeric forms so 1, 1.0 and 1e0 all freeze identically.
func canonicalJSON(v any) ([]byte, error) {
	return json.Marshal(normalizeNumbers(v))
}

func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = normalizeNumbers(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = normalizeNumbers(val)
		}
		return out
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
		}
		return x.String()
	default:
		return v
	}
}

// Request freezes a high-risk write and returns an existing open approval for
// the same principal+tool+hash instead of stacking a duplicate.
func (m *ApprovalManager) Request(a Actor, endpoint, serviceID, tool string, raw json.RawMessage, requestID string) (*store.MCPApproval, *Error) {
	canonical, hash, err := FreezeParams(tool, raw)
	if err != nil {
		return nil, InvalidParams("%s", err.Error())
	}
	now := m.now().Unix()
	existing, err := m.st.GetOpenMCPApproval(a.Principal(), tool, hash)
	if err != nil {
		return nil, NewError(CodeInternal, "could not create approval", 500)
	}
	if existing != nil {
		if existing.Status == "pending" && existing.ExpiresAt <= now {
			_, _ = m.st.ExpireMCPApproval(existing.ID, now)
		} else if existing.Status == "approved" && existing.ExecuteBefore <= now {
			_, _ = m.st.ExpireMCPApproval(existing.ID, now)
		} else {
			return existing, nil
		}
	}
	id, err := NewID("mcpap_")
	if err != nil {
		return nil, NewError(CodeInternal, "could not create approval", 500)
	}
	a2 := &store.MCPApproval{
		ID: id, KeyID: a.KeyID, KeyName: a.Name, Principal: a.Principal(),
		AuthKind: a.AuthKind, Endpoint: endpoint, ServiceID: serviceID, Tool: tool,
		Risk: RiskHigh, ParamsJSON: string(canonical), ParamsHash: hash,
		Status: "pending", RequestedAt: now, ExpiresAt: now + int64(m.cfg.approvalPendingTTLFor(serviceID)),
		RequestID: requestID,
	}
	if err := m.st.CreateMCPApproval(a2); err != nil {
		// Lost a race against the partial unique index: return the winner.
		if other, gerr := m.st.GetOpenMCPApproval(a.Principal(), tool, hash); gerr == nil && other != nil {
			return other, nil
		}
		return nil, NewError(CodeInternal, "could not create approval", 500)
	}
	return a2, nil
}

// Approve CASes pending -> approved and sets the 10-minute execution window.
func (m *ApprovalManager) Approve(id, by, note string) (*store.MCPApproval, *Error) {
	now := m.now().Unix()
	row, err := m.st.GetMCPApproval(id)
	if err != nil {
		return nil, NewError(CodeInternal, "approval lookup failed", 500)
	}
	if row == nil {
		return nil, NewError(CodeApprovalNotFound, "approval not found", 404)
	}
	if row.Status == "pending" && row.ExpiresAt <= now {
		_, _ = m.st.ExpireMCPApproval(id, now)
		return nil, NewError(CodeApprovalExpired, "approval expired", 200)
	}
	ok, err := m.st.ApproveMCPApproval(id, now, now+int64(m.cfg.approvalTTLFor(row.ServiceID)), by, note)
	if err != nil {
		return nil, NewError(CodeInternal, "approval failed", 500)
	}
	if !ok {
		return nil, m.conflict(id)
	}
	row, err = m.st.GetMCPApproval(id)
	if err != nil || row == nil {
		return nil, NewError(CodeInternal, "approval lookup failed", 500)
	}
	return row, nil
}

// Reject CASes pending -> rejected.
func (m *ApprovalManager) Reject(id, by, note string) (*store.MCPApproval, *Error) {
	now := m.now().Unix()
	row, err := m.st.GetMCPApproval(id)
	if err != nil {
		return nil, NewError(CodeInternal, "approval lookup failed", 500)
	}
	if row == nil {
		return nil, NewError(CodeApprovalNotFound, "approval not found", 404)
	}
	if row.Status == "pending" && row.ExpiresAt <= now {
		_, _ = m.st.ExpireMCPApproval(id, now)
		return nil, NewError(CodeApprovalExpired, "approval expired", 200)
	}
	ok, err := m.st.RejectMCPApproval(id, now, by, note)
	if err != nil {
		return nil, NewError(CodeInternal, "approval failed", 500)
	}
	if !ok {
		return nil, m.conflict(id)
	}
	row, err = m.st.GetMCPApproval(id)
	if err != nil || row == nil {
		return nil, NewError(CodeInternal, "approval lookup failed", 500)
	}
	return row, nil
}

// ClaimForExecution validates ownership + re-frozen params, then CASes
// approved -> executed BEFORE the upstream call (one-shot, replay-safe).
func (m *ApprovalManager) ClaimForExecution(id string, a Actor, endpoint, serviceID, tool string, raw json.RawMessage) (*store.MCPApproval, *Error) {
	row, err := m.st.GetMCPApproval(id)
	if err != nil {
		return nil, NewError(CodeInternal, "approval lookup failed", 500)
	}
	if row == nil {
		return nil, NewError(CodeApprovalNotFound, "approval not found", 404)
	}
	if row.Principal != a.Principal() {
		return nil, NewError(CodeApprovalNotYours, "approval belongs to another principal", 403)
	}
	if a.Kind == ActorKey && row.KeyID != "" && row.KeyID != a.KeyID {
		return nil, NewError(CodeApprovalNotYours, "approval belongs to another key", 403)
	}
	if row.ServiceID != serviceID || row.Tool != tool {
		return nil, NewError(CodeParamsMismatch, "approval was frozen for a different tool", 200)
	}
	if row.Endpoint != "" && endpoint != "" && row.Endpoint != endpoint {
		return nil, NewError(CodeParamsMismatch, "approval was frozen for a different endpoint", 200)
	}
	_, hash, ferr := FreezeParams(tool, raw)
	if ferr != nil {
		return nil, InvalidParams("%s", ferr.Error())
	}
	if hash != row.ParamsHash {
		return nil, NewError(CodeParamsMismatch, "params differ from the frozen approval", 200)
	}
	now := m.now().Unix()
	switch row.Status {
	case "pending":
		if row.ExpiresAt <= now {
			_, _ = m.st.ExpireMCPApproval(id, now)
			return nil, NewError(CodeApprovalExpired, "approval expired", 200)
		}
		return nil, NewError(CodeApprovalPending, "approval is still pending", 200)
	case "rejected":
		return nil, NewError(CodeApprovalRejected, "approval was rejected", 200)
	case "expired":
		return nil, NewError(CodeApprovalExpired, "approval expired", 200)
	case "executed":
		return nil, NewError(CodeApprovalUsed, "approval has already been used", 200)
	case "approved":
		if row.ExecuteBefore <= now {
			_, _ = m.st.ExpireMCPApproval(id, now)
			return nil, NewError(CodeApprovalExpired, "execution window expired", 200)
		}
		ok, cerr := m.st.ClaimMCPApprovalExecution(id, now)
		if cerr != nil {
			return nil, NewError(CodeInternal, "approval execution failed", 500)
		}
		if !ok {
			latest, _ := m.st.GetMCPApproval(id)
			if latest != nil && latest.Status == "executed" {
				return nil, NewError(CodeApprovalUsed, "approval has already been used", 200)
			}
			if latest != nil && latest.Status == "expired" {
				return nil, NewError(CodeApprovalExpired, "execution window expired", 200)
			}
			return nil, NewError(CodeApprovalConflict, "approval was decided concurrently", 409)
		}
		row, _ = m.st.GetMCPApproval(id)
		return row, nil
	default:
		return nil, NewError(CodeApprovalConflict, "approval is in an unknown state", 409)
	}
}

// Complete records the upstream outcome on the already-terminal executed row.
func (m *ApprovalManager) Complete(id, outcome, errCode string, upstreamStatus int) {
	_ = m.st.UpdateMCPApprovalOutcome(id, outcome, errCode, upstreamStatus)
}

// Conflict maps a failed CAS to the current row's terminal state.
func (m *ApprovalManager) conflict(id string) *Error {
	row, err := m.st.GetMCPApproval(id)
	if err != nil || row == nil {
		return NewError(CodeApprovalNotFound, "approval not found", 404)
	}
	switch row.Status {
	case "rejected":
		return NewError(CodeApprovalRejected, "approval was rejected", 200)
	case "expired":
		return NewError(CodeApprovalExpired, "approval expired", 200)
	case "executed":
		return NewError(CodeApprovalUsed, "approval has already been used", 200)
	default:
		return NewError(CodeApprovalConflict, "approval was already decided", 409)
	}
}

// ExpireDue transitions due pending/approved rows and returns them for audit.
func (m *ApprovalManager) ExpireDue(now time.Time) ([]*store.MCPApproval, error) {
	return m.st.ExpireDueMCPApprovals(now.Unix())
}

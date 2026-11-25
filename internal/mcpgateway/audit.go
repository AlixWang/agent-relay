package mcpgateway

import (
	"fmt"
	"log"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Auditor writes mcp_audit and mirrors a compact row into the existing
// audit_log (#31: both). Failure to audit must never fail the call; the
// dedicated table is the source of truth for the console.
type Auditor struct {
	st  store.Store
	cfg Config
	now func() time.Time
}

func NewAuditor(st store.Store, cfg Config) *Auditor {
	return &Auditor{st: st, cfg: cfg, now: time.Now}
}

func (a *Auditor) SetClock(now func() time.Time) {
	if now != nil {
		a.now = now
	}
}

// Record appends the gateway row and best-effort mirrors it into audit_log.
func (a *Auditor) Record(e *store.MCPAuditEntry) {
	if e == nil {
		return
	}
	if e.Ts == 0 {
		e.Ts = a.now().Unix()
	}
	if err := a.st.AppendMCPAudit(e); err != nil {
		log.Printf("mcp audit: append: %v", err)
		return
	}
	action := auditAction(e)
	detail := fmt.Sprintf("service=%s tool=%s risk=%s outcome=%s code=%s approval=%s upstream=%d",
		e.ServiceID, e.Tool, e.Risk, e.Outcome, e.ErrorCode, e.ApprovalID, e.UpstreamStatus)
	if err := a.st.AppendAudit(e.Actor, action, detail, e.Ts); err != nil {
		log.Printf("mcp audit: mirror: %v", err)
	}
}

// RecordAdmin writes an operator action to both stores. The gateway audit
// table has no action column, so the action name goes into detail.
func (a *Auditor) RecordAdmin(action, detail string) {
	now := a.now().Unix()
	if err := a.st.AppendAudit("admin", action, detail, now); err != nil {
		log.Printf("mcp admin audit: %v", err)
	}
	if err := a.st.AppendMCPAudit(&store.MCPAuditEntry{
		Ts: now, Actor: "admin", AuthKind: AuthAdmin, Outcome: "ok",
		Risk: "admin", Detail: action + " " + detail,
	}); err != nil {
		log.Printf("mcp admin audit row: %v", err)
	}
}

// auditAction maps a gateway row to the v5 audit action names. Only the five
// write lifecycle names are special; everything else is mcp.call.
func auditAction(e *store.MCPAuditEntry) string {
	if e.ApprovalID == "" {
		return "mcp.call"
	}
	switch e.Outcome {
	case "pending":
		return "mcp.write_request"
	case "approved":
		return "mcp.write_approved"
	case "rejected":
		return "mcp.write_rejected"
	case "expired":
		return "mcp.write_expired"
	case "executed", "ok", "upstream_error", "timeout", "error":
		return "mcp.write_executed"
	default:
		return "mcp.call"
	}
}

// List returns console rows.
func (a *Auditor) List(f store.MCPAuditFilter, limit, offset int) ([]*store.MCPAuditEntry, int, error) {
	return a.st.ListMCPAudit(f, limit, offset)
}

// MaintenanceResult reports one retention run.
type MaintenanceResult struct {
	PendingExpired  int
	ApprovedExpired int
	Pruned          int64
	Trimmed         int64
}

// RunMaintenance expires due approvals (auditing each) and applies the
// 90/180-day + 2M-row audit policy (#37). It is called by the server's
// background loop, not on the request path.
func (a *Auditor) RunMaintenance(am *ApprovalManager, now time.Time) (MaintenanceResult, error) {
	var out MaintenanceResult
	expired, err := am.ExpireDue(now)
	if err != nil {
		return out, err
	}
	for _, row := range expired {
		if row.Status == "expired" && row.ExecuteBefore == 0 {
			out.PendingExpired++
		} else {
			out.ApprovedExpired++
		}
		a.Record(&store.MCPAuditEntry{
			Ts: now.Unix(), RequestID: row.RequestID, Actor: row.Principal,
			AuthKind: row.AuthKind, KeyID: row.KeyID, KeyName: row.KeyName,
			Endpoint: row.Endpoint, ServiceID: row.ServiceID, Tool: row.Tool,
			Risk: row.Risk, ParamsHash: row.ParamsHash, ApprovalID: row.ID,
			Outcome: "expired", ErrorCode: CodeApprovalExpired,
			Detail: "expired by retention",
		})
	}
	// Zero days disables pruning for that class; a zero cutoff must not
	// become "delete everything older than now".
	var readBefore, writeBefore int64
	if a.cfg.AuditRetentionDaysRead > 0 {
		readBefore = now.Add(-time.Duration(a.cfg.AuditRetentionDaysRead) * 24 * time.Hour).Unix()
	}
	if a.cfg.AuditRetentionDaysWrite > 0 {
		writeBefore = now.Add(-time.Duration(a.cfg.AuditRetentionDaysWrite) * 24 * time.Hour).Unix()
	}
	if readBefore != 0 || writeBefore != 0 {
		n, err := a.st.PruneMCPAudit(readBefore, writeBefore)
		if err != nil {
			return out, err
		}
		out.Pruned = n
	}
	if a.cfg.AuditMaxRows > 0 {
		n, err := a.st.TrimMCPAudit(int64(a.cfg.AuditMaxRows))
		if err != nil {
			return out, err
		}
		out.Trimmed = n
	}
	return out, nil
}

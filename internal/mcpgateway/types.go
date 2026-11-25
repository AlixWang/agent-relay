// Package mcpgateway is the in-process relay MCP gateway (design v5).
//
// One endpoint family serves two identities: relay assistants authenticate
// with their relay token and are authorized by their mcp_keys peer row;
// external MCP clients authenticate with a Bearer rmk_ key. Services are
// explicitly registered and own only upstream calls + parameter validation;
// authn, authz, limits, approvals, and audit live here.
package mcpgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Version is reported in initialize.serverInfo and /mcp/health.
const Version = "5.0.0"

// Risk classes (design v5 §1.3/§5.4).
const (
	RiskRead = "read"
	RiskLow  = "low"
	RiskHigh = "high"
)

// Actor kinds.
const (
	ActorPeer  = "peer"
	ActorKey   = "key"
	ActorAdmin = "admin"
)

// Auth kinds recorded in audit/approval rows.
const (
	AuthRelayToken = "relay_token"
	AuthMCPKey     = "mcp_key"
	AuthAdmin      = "admin"
)

// Error codes (design v5 §1.2/§1.4).
const (
	CodeUnauthorized       = "unauthorized"
	CodeInsufficientScope  = "insufficient_scope"
	CodeServiceDisabled    = "service_disabled"
	CodeToolDisabled       = "tool_disabled"
	CodeUnknownTool        = "unknown_tool"
	CodeInvalidParams      = "invalid_params"
	CodeRateLimited        = "rate_limited"
	CodeQuotaExceeded      = "quota_exceeded"
	CodeApprovalRejected   = "approval_rejected"
	CodeApprovalExpired    = "approval_expired"
	CodeApprovalUsed       = "approval_already_used"
	CodeApprovalNotYours   = "approval_not_yours"
	CodeApprovalPending    = "approval_pending"
	CodeApprovalNotFound   = "approval_not_found"
	CodeApprovalConflict   = "approval_conflict"
	CodeParamsMismatch     = "params_mismatch"
	CodeUpstreamError      = "upstream_error"
	CodeUpstreamTimeout    = "upstream_timeout"
	CodeCredentialMissing  = "credential_missing"
	CodeNotImplemented     = "not_implemented"
	CodeInternal           = "internal_error"
	CodeRequestTooLarge    = "request_too_large"
	CodeUnsupportedVersion = "unsupported_protocol_version"
)

// MCPService is implemented by every explicitly registered service package.
type MCPService interface {
	ID() string
	DisplayName() string
	Version() string
	Tools() []Tool
	CredentialRefs() []string
	Handle(ctx context.Context, call Call) (Result, error)
}

// Prefixer lets a service choose its aggregate-endpoint tool prefix. The
// fallback is a sanitized service id (bing-webmaster -> bing_webmaster), so
// services with a shorter prefix (bing) implement this.
type Prefixer interface {
	Prefix() string
}

// Tool describes one exposed tool. Name is the short name for the
// /mcp/<service> endpoint; the registry adds the aggregate prefix.
type Tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	Risk         string          `json:"risk"`
	ReadOnly     bool            `json:"-"`
	Destructive  bool            `json:"-"`
	Idempotent   bool            `json:"-"`
	OpenWorld    bool            `json:"-"`
	InputSchema  json.RawMessage `json:"inputSchema,omitempty"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
	// Validate runs after the generic envelope checks and before authz/rate
	// limiting. Services set it to enforce their own whitelist. It must
	// return *Error(CodeInvalidParams) (or a plain error, wrapped here).
	Validate func(raw json.RawMessage) error `json:"-"`
}

// Call is the only thing a service sees about a request. It never carries
// credentials or raw HTTP objects.
type Call struct {
	Actor      Actor
	ServiceID  string
	Tool       string
	Params     json.RawMessage
	ParamsHash string
	ApprovalID string
	DryRun     bool
	RequestID  string
	Endpoint   string
}

// Actor is the authenticated caller.
type Actor struct {
	Kind     string
	ID       string
	Name     string
	KeyID    string
	PeerID   string
	IP       string
	AuthKind string
	Scopes   []string
}

// Principal is the stable approval owner string. Keys are bound by id (not
// display name) so two operator-created keys that share a name can never
// execute each other's approval. Audit rows use AuditActor for readability.
func (a Actor) Principal() string {
	switch a.Kind {
	case ActorPeer:
		return "peer:" + a.PeerID
	case ActorKey:
		id := a.KeyID
		if id == "" {
			id = a.ID
		}
		return "key:" + id
	default:
		return a.Kind + ":" + a.ID
	}
}

// AuditActor is the human-readable identity recorded in mcp_audit/audit_log.
func (a Actor) AuditActor() string {
	if a.Kind == ActorKey && a.Name != "" {
		return "key:" + a.Name
	}
	return a.Principal()
}

// Result is a successful tool result. Data is wrapped in the v5 envelope by
// the server; services never write JSON-RPC or HTTP.
type Result struct {
	Data           any
	Truncated      bool
	RowCount       int
	UpstreamStatus int
	Latency        time.Duration
}

// Error is a typed gateway/tool failure. Tool failures are returned to the
// client as HTTP 200 + isError (except authz/rate preconditions, which the
// server maps to HTTP 401/403/429).
type Error struct {
	Code       string         `json:"code"`
	HTTPStatus int            `json:"-"`
	Message    string         `json:"message"`
	Details    map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// NewError builds a typed error. httpStatus 0 defaults to 400 for invalid
// params, 502 for upstream errors, 403 for authz, 429 for rate limits.
func NewError(code, message string, httpStatus int) *Error {
	return &Error{Code: code, Message: message, HTTPStatus: httpStatus}
}

// AsError converts a plain service error into a typed one without inventing
// detail that could leak upstream/credential material.
func AsError(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := err.(*Error); ok {
		return e
	}
	return &Error{Code: CodeInternal, Message: "tool execution failed", HTTPStatus: 500}
}

// InvalidParams is the common service-side validation error.
func InvalidParams(format string, args ...any) *Error {
	return &Error{Code: CodeInvalidParams, Message: fmt.Sprintf(format, args...), HTTPStatus: 400}
}

// Schema builds the object input schema used by services. It is intentionally
// small (no dependency): properties is a map of JSON-schema fragments, and
// additionalProperties=false makes unknown fields fail before a service sees
// them (design v5 §1.4).
func Schema(properties map[string]any, required ...string) json.RawMessage {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	b, _ := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	return b
}

// Prop is a compact JSON-schema property constructor for service tool tables.
func Prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

// ArrayProp describes an array property with a scalar item type.
func ArrayProp(itemType, description string) map[string]any {
	return map[string]any{
		"type": "array", "description": description,
		"items": map[string]any{"type": itemType},
	}
}

// EnumProp describes a string enum.
func EnumProp(description string, values ...string) map[string]any {
	return map[string]any{"type": "string", "description": description, "enum": values}
}

var toolNameRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
var serviceIDRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var prefixRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// ValidToolName reports whether name is a legal short tool name.
func ValidToolName(name string) bool { return toolNameRe.MatchString(name) }

// ValidServiceID reports whether id is a legal service id.
func ValidServiceID(id string) bool { return serviceIDRe.MatchString(id) }

// ValidPrefix reports whether prefix is a legal aggregate prefix (without the
// trailing underscore).
func ValidPrefix(prefix string) bool { return prefixRe.MatchString(prefix) }

// AggregateName is prefix + "_" + short tool name.
func AggregateName(prefix, tool string) string { return strings.ToLower(prefix) + "_" + tool }

// WriteTool reports whether the tool's risk class is a write class.
func WriteTool(t Tool) bool { return t.Risk == RiskLow || t.Risk == RiskHigh }

// RiskClass maps a tool to "read" or "write" for scope matching.
func RiskClass(t Tool) string {
	if WriteTool(t) {
		return "write"
	}
	return "read"
}

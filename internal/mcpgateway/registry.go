package mcpgateway

import (
	"fmt"
	"sort"
	"strings"
)

// Registry is the explicit service registry (design v5 §3.3). Registration is
// done at wiring time, never via init(), and any conflict fails startup.
type Registry struct {
	services map[string]*registeredService
	byPrefix map[string]*registeredService
	aggr     map[string]*ResolvedTool
	order    []string
}

type registeredService struct {
	svc    MCPService
	prefix string
	tools  map[string]Tool
	order  []string
}

// ServiceInfo is the safe admin view of one registered service.
type ServiceInfo struct {
	ID          string
	DisplayName string
	Version     string
	Prefix      string
	ToolCount   int
	Tools       []Tool
}

// ResolvedTool is one tool resolved from an endpoint name.
type ResolvedTool struct {
	Service       MCPService
	ServiceID     string
	Prefix        string
	Name          string
	AggregateName string
	Tool          Tool
}

func NewRegistry() *Registry {
	return &Registry{
		services: map[string]*registeredService{},
		byPrefix: map[string]*registeredService{},
		aggr:     map[string]*ResolvedTool{},
	}
}

// Register adds a service and validates every service/tool/prefix invariant.
func (r *Registry) Register(svc MCPService) error {
	if svc == nil {
		return fmt.Errorf("registry: nil service")
	}
	id := svc.ID()
	if !ValidServiceID(id) {
		return fmt.Errorf("registry: invalid service id %q", id)
	}
	if _, dup := r.services[id]; dup {
		return fmt.Errorf("registry: duplicate service id %q", id)
	}
	prefix := sanitizedPrefix(id)
	if p, ok := svc.(Prefixer); ok {
		prefix = p.Prefix()
	}
	if !ValidPrefix(prefix) {
		return fmt.Errorf("registry: service %q has invalid aggregate prefix %q", id, prefix)
	}
	if other, dup := r.byPrefix[prefix]; dup {
		return fmt.Errorf("registry: aggregate prefix %q used by both %q and %q", prefix, other.svc.ID(), id)
	}
	tools := svc.Tools()
	if len(tools) == 0 {
		return fmt.Errorf("registry: service %q exposes no tools", id)
	}
	reg := &registeredService{svc: svc, prefix: prefix, tools: map[string]Tool{}}
	for _, t := range tools {
		if !ValidToolName(t.Name) {
			return fmt.Errorf("registry: service %q has invalid tool name %q", id, t.Name)
		}
		switch t.Risk {
		case RiskRead, RiskLow, RiskHigh:
		default:
			return fmt.Errorf("registry: service %q tool %q has invalid risk %q", id, t.Name, t.Risk)
		}
		if t.ReadOnly != (t.Risk == RiskRead) {
			return fmt.Errorf("registry: service %q tool %q: readOnly must match risk", id, t.Name)
		}
		if _, dup := reg.tools[t.Name]; dup {
			return fmt.Errorf("registry: service %q duplicate tool %q", id, t.Name)
		}
		agg := AggregateName(prefix, t.Name)
		if len(agg) > 64 {
			return fmt.Errorf("registry: aggregate tool name %q exceeds 64 characters", agg)
		}
		if other, dup := r.aggr[agg]; dup {
			return fmt.Errorf("registry: aggregate tool name %q collides with %s/%s", agg, other.ServiceID, other.Name)
		}
		reg.tools[t.Name] = t
		reg.order = append(reg.order, t.Name)
		r.aggr[agg] = &ResolvedTool{
			Service: svc, ServiceID: id, Prefix: prefix,
			Name: t.Name, AggregateName: agg, Tool: t,
		}
	}
	r.services[id] = reg
	r.byPrefix[prefix] = reg
	r.order = append(r.order, id)
	return nil
}

func sanitizedPrefix(id string) string {
	return strings.ReplaceAll(id, "-", "_")
}

// Get returns one service by id.
func (r *Registry) Get(id string) (MCPService, bool) {
	rs, ok := r.services[id]
	if !ok {
		return nil, false
	}
	return rs.svc, true
}

// ServiceInfo returns the registered service metadata.
func (r *Registry) ServiceInfo(id string) (ServiceInfo, bool) {
	rs, ok := r.services[id]
	if !ok {
		return ServiceInfo{}, false
	}
	tools := make([]Tool, 0, len(rs.order))
	for _, name := range rs.order {
		tools = append(tools, rs.tools[name])
	}
	return ServiceInfo{
		ID: rs.svc.ID(), DisplayName: rs.svc.DisplayName(), Version: rs.svc.Version(),
		Prefix: rs.prefix, ToolCount: len(tools), Tools: tools,
	}, true
}

// List returns registered services sorted by id.
func (r *Registry) List() []ServiceInfo {
	ids := append([]string(nil), r.order...)
	sort.Strings(ids)
	out := make([]ServiceInfo, 0, len(ids))
	for _, id := range ids {
		info, _ := r.ServiceInfo(id)
		out = append(out, info)
	}
	return out
}

// Tool returns one service's short-name tool.
func (r *Registry) Tool(serviceID, name string) (Tool, bool) {
	rs, ok := r.services[serviceID]
	if !ok {
		return Tool{}, false
	}
	t, ok := rs.tools[name]
	return t, ok
}

// ToolsFor returns a service's tools in registration order under a copy.
func (r *Registry) ToolsFor(serviceID string) []Tool {
	rs, ok := r.services[serviceID]
	if !ok {
		return nil
	}
	out := make([]Tool, 0, len(rs.order))
	for _, name := range rs.order {
		out = append(out, rs.tools[name])
	}
	return out
}

// AggregateTools returns aggregate-prefixed tool copies, sorted by name.
func (r *Registry) AggregateTools() []Tool {
	out := make([]Tool, 0, len(r.aggr))
	for _, rt := range r.aggr {
		t := rt.Tool
		t.Name = rt.AggregateName
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResolveTool maps a wire tool name to its service+tool. aggregate selects the
// /mcp endpoint (prefixed names); otherwise the /mcp/<service> endpoint
// (short names). A prefixed name on a service endpoint (or vice versa) is not
// found, matching design v5 §1.3.
func (r *Registry) ResolveTool(serviceID, name string, aggregate bool) (*ResolvedTool, bool) {
	if aggregate {
		rt, ok := r.aggr[name]
		return rt, ok
	}
	rs, ok := r.services[serviceID]
	if !ok {
		return nil, false
	}
	t, ok := rs.tools[name]
	if !ok {
		return nil, false
	}
	return &ResolvedTool{
		Service: rs.svc, ServiceID: serviceID, Prefix: rs.prefix,
		Name: name, AggregateName: AggregateName(rs.prefix, name), Tool: t,
	}, true
}

// Prefix returns a service's aggregate prefix (used by admin tools view).
func (r *Registry) Prefix(serviceID string) string {
	if rs, ok := r.services[serviceID]; ok {
		return rs.prefix
	}
	return ""
}

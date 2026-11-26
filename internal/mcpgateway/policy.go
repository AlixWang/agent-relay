package mcpgateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
)

const runtimePolicySettingKey = "mcp.runtime_policy_v1"

var (
	errUnknownService = errors.New("unknown mcp service")
	errPolicyPersist  = errors.New("could not persist mcp runtime policy")
)

// runtimePolicy holds operator console overrides layered on top of config.toml
// defaults and persisted in the store settings table.
type runtimePolicy struct {
	Enabled      *bool                           `json:"enabled,omitempty"`
	WriteEnabled *bool                           `json:"write_enabled,omitempty"`
	Services     map[string]runtimeServicePolicy `json:"services,omitempty"`
}

type runtimeServicePolicy struct {
	Enabled           *bool     `json:"enabled,omitempty"`
	AllowedTools      *[]string `json:"allowed_tools,omitempty"`
	WriteAllowedTools *[]string `json:"write_allowed_tools,omitempty"`
}

func (p runtimeServicePolicy) isEmpty() bool {
	return p.Enabled == nil && p.AllowedTools == nil && p.WriteAllowedTools == nil
}

func (p runtimePolicy) isEmpty() bool {
	if p.Enabled != nil || p.WriteEnabled != nil {
		return false
	}
	for _, svc := range p.Services {
		if !svc.isEmpty() {
			return false
		}
	}
	return true
}

func copyStringSlice(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func (p runtimePolicy) clone() runtimePolicy {
	out := runtimePolicy{}
	if p.Enabled != nil {
		v := *p.Enabled
		out.Enabled = &v
	}
	if p.WriteEnabled != nil {
		v := *p.WriteEnabled
		out.WriteEnabled = &v
	}
	if len(p.Services) > 0 {
		out.Services = make(map[string]runtimeServicePolicy, len(p.Services))
		for id, svc := range p.Services {
			cp := runtimeServicePolicy{}
			if svc.Enabled != nil {
				v := *svc.Enabled
				cp.Enabled = &v
			}
			if svc.AllowedTools != nil {
				list := copyStringSlice(*svc.AllowedTools)
				cp.AllowedTools = &list
			}
			if svc.WriteAllowedTools != nil {
				list := copyStringSlice(*svc.WriteAllowedTools)
				cp.WriteAllowedTools = &list
			}
			out.Services[id] = cp
		}
	}
	return out
}

func mergeRuntimePolicy(base Config, p runtimePolicy) Config {
	cfg := base.Clone()
	if p.Enabled != nil {
		cfg.Enabled = *p.Enabled
	}
	if p.WriteEnabled != nil {
		cfg.Write.Enabled = *p.WriteEnabled
	}
	for id, ov := range p.Services {
		if ov.Enabled != nil || ov.AllowedTools != nil {
			if cfg.Services == nil {
				cfg.Services = map[string]ServiceConfig{}
			}
			svc, ok := cfg.Services[id]
			if !ok {
				svc = ServiceConfig{Enabled: true, AllowedTools: []string{"*"}}
			}
			if ov.Enabled != nil {
				svc.Enabled = *ov.Enabled
			}
			if ov.AllowedTools != nil {
				svc.AllowedTools = copyStringSlice(*ov.AllowedTools)
			}
			cfg.Services[id] = svc
		}
		if ov.WriteAllowedTools != nil {
			if cfg.Write.Services == nil {
				cfg.Write.Services = map[string]WriteServiceConfig{}
			}
			wsvc := cfg.Write.Services[id]
			wsvc.AllowedTools = copyStringSlice(*ov.WriteAllowedTools)
			cfg.Write.Services[id] = wsvc
		}
	}
	return cfg
}

// sanitizeRuntimePolicy drops references to services or tools that are not in
// reg so stale database overrides from an older binary cannot block startup.
func sanitizeRuntimePolicy(reg *Registry, p runtimePolicy) (runtimePolicy, bool) {
	if reg == nil || p.isEmpty() {
		return p, false
	}
	cp := p.clone()
	changed := false
	for id, ov := range cp.Services {
		if _, ok := reg.Get(id); !ok {
			log.Printf("WARN: pruning unknown service %q from persisted mcp runtime policy", id)
			delete(cp.Services, id)
			changed = true
			continue
		}
		if ov.AllowedTools != nil {
			orig := *ov.AllowedTools
			if !(len(orig) == 1 && orig[0] == "*") {
				filtered := make([]string, 0, len(orig))
				seen := map[string]bool{}
				for _, name := range orig {
					t, ok := reg.Tool(id, name)
					if !ok || WriteTool(t) {
						log.Printf("WARN: pruning invalid read tool %q for service %q from persisted mcp runtime policy", name, id)
						changed = true
						continue
					}
					if !seen[name] {
						seen[name] = true
						filtered = append(filtered, name)
					}
				}
				if len(filtered) != len(orig) {
					changed = true
				}
				ov.AllowedTools = &filtered
			}
		}
		if ov.WriteAllowedTools != nil {
			orig := *ov.WriteAllowedTools
			filtered := make([]string, 0, len(orig))
			seen := map[string]bool{}
			for _, name := range orig {
				t, ok := reg.Tool(id, name)
				if !ok || !WriteTool(t) {
					log.Printf("WARN: pruning invalid write tool %q for service %q from persisted mcp runtime policy", name, id)
					changed = true
					continue
				}
				if !seen[name] {
					seen[name] = true
					filtered = append(filtered, name)
				}
			}
			if len(filtered) != len(orig) {
				changed = true
			}
			ov.WriteAllowedTools = &filtered
		}
		if ov.isEmpty() {
			delete(cp.Services, id)
		} else {
			cp.Services[id] = ov
		}
	}
	return cp, changed
}

func (s *Server) loadRuntimePolicy() {
	raw, err := s.st.GetSetting(runtimePolicySettingKey)
	if err != nil {
		log.Printf("WARN: could not read persisted mcp runtime policy: %v", err)
		return
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}
	var p runtimePolicy
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		log.Printf("WARN: ignoring corrupt persisted mcp runtime policy: %v", err)
		return
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	s.policy = p.clone()
	s.cfg = mergeRuntimePolicy(s.baseCfg, s.policy)
}

func (s *Server) saveRuntimePolicyLocked(p runtimePolicy) error {
	var err error
	if p.isEmpty() {
		err = s.st.SetSetting(runtimePolicySettingKey, "", s.now().Unix())
	} else {
		var b []byte
		b, err = json.Marshal(p)
		if err != nil {
			return fmt.Errorf("%w: %v", errPolicyPersist, err)
		}
		err = s.st.SetSetting(runtimePolicySettingKey, string(b), s.now().Unix())
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errPolicyPersist, err)
	}
	return nil
}

func (s *Server) applyRuntimeConfigUpdate(enabled, writeEnabled *bool) (Config, bool, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	next := s.policy.clone()
	if enabled != nil {
		v := *enabled
		if s.baseCfg.Enabled == v {
			next.Enabled = nil
		} else {
			next.Enabled = &v
		}
	}
	if writeEnabled != nil {
		v := *writeEnabled
		if s.baseCfg.Write.Enabled == v {
			next.WriteEnabled = nil
		} else {
			next.WriteEnabled = &v
		}
	}
	if err := s.saveRuntimePolicyLocked(next); err != nil {
		return Config{}, false, err
	}
	s.policy = next
	s.cfg = mergeRuntimePolicy(s.baseCfg, s.policy)
	return s.cfg.Clone(), !s.policy.isEmpty(), nil
}

func (s *Server) resetRuntimePolicy() (Config, error) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if err := s.saveRuntimePolicyLocked(runtimePolicy{}); err != nil {
		return Config{}, err
	}
	s.policy = runtimePolicy{}
	s.cfg = s.baseCfg.Clone()
	return s.cfg.Clone(), nil
}

func validateReadToolSelection(reg *Registry, serviceID string, list []string) ([]string, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("allowed_tools must not be empty (disable the service instead)")
	}
	if len(list) == 1 && strings.TrimSpace(list[0]) == "*" {
		return []string{"*"}, nil
	}
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("allowed_tools must not contain empty tool names")
		}
		if name == "*" {
			return nil, fmt.Errorf("allowed_tools cannot combine \"*\" with explicit tool names")
		}
		t, ok := reg.Tool(serviceID, name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q for service %q", name, serviceID)
		}
		if WriteTool(t) {
			return nil, fmt.Errorf("%q is a write tool; configure it via write_allowed_tools", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func validateWriteToolSelection(reg *Registry, serviceID string, list []string) ([]string, error) {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, raw := range list {
		name := strings.TrimSpace(raw)
		if name == "" {
			return nil, fmt.Errorf("write_allowed_tools must not contain empty tool names")
		}
		if name == "*" {
			return nil, fmt.Errorf("write_allowed_tools must list explicit tools, not \"*\"")
		}
		t, ok := reg.Tool(serviceID, name)
		if !ok {
			return nil, fmt.Errorf("unknown tool %q for service %q", name, serviceID)
		}
		if !WriteTool(t) {
			return nil, fmt.Errorf("%q is a read tool; configure it via allowed_tools", name)
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, nil
}

func stringSlicesEqual(a, b []string) bool {
	return slices.Equal(a, b)
}

func (s *Server) applyRuntimeServiceUpdate(serviceID string, enabled *bool, allowedTools, writeAllowedTools *[]string) (Config, error) {
	if _, ok := s.registry.Get(serviceID); !ok {
		return Config{}, errUnknownService
	}
	var validatedRead *[]string
	if allowedTools != nil {
		clean, err := validateReadToolSelection(s.registry, serviceID, *allowedTools)
		if err != nil {
			return Config{}, err
		}
		validatedRead = &clean
	}
	var validatedWrite *[]string
	if writeAllowedTools != nil {
		clean, err := validateWriteToolSelection(s.registry, serviceID, *writeAllowedTools)
		if err != nil {
			return Config{}, err
		}
		validatedWrite = &clean
	}

	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	next := s.policy.clone()
	if next.Services == nil {
		next.Services = map[string]runtimeServicePolicy{}
	}
	ov := next.Services[serviceID]
	baseSvc, hasBaseSvc := s.baseCfg.Services[serviceID]
	baseWrite := s.baseCfg.writeService(serviceID)

	if enabled != nil {
		v := *enabled
		if hasBaseSvc && baseSvc.Enabled == v {
			ov.Enabled = nil
		} else {
			ov.Enabled = &v
		}
	}
	if validatedRead != nil {
		if hasBaseSvc && stringSlicesEqual(baseSvc.AllowedTools, *validatedRead) {
			ov.AllowedTools = nil
		} else {
			cp := copyStringSlice(*validatedRead)
			ov.AllowedTools = &cp
		}
	}
	if validatedWrite != nil {
		if stringSlicesEqual(baseWrite.AllowedTools, *validatedWrite) {
			ov.WriteAllowedTools = nil
		} else {
			cp := copyStringSlice(*validatedWrite)
			ov.WriteAllowedTools = &cp
		}
	}
	if ov.isEmpty() {
		delete(next.Services, serviceID)
	} else {
		next.Services[serviceID] = ov
	}
	if len(next.Services) == 0 {
		next.Services = nil
	}

	if err := s.saveRuntimePolicyLocked(next); err != nil {
		return Config{}, err
	}
	s.policy = next
	s.cfg = mergeRuntimePolicy(s.baseCfg, s.policy)
	return s.cfg.Clone(), nil
}

// serviceCredentialStatus probes whether the service requires a credential and
// whether it resolves right now. Plaintext is never returned or logged.
func (s *Server) serviceCredentialStatus(info ServiceInfo, cfgSvc ServiceConfig) (configured bool, required bool) {
	required = cfgSvc.CredentialRef != "" || len(info.CredentialRefs) > 0
	if !required {
		return true, false
	}
	if cfgSvc.CredentialRef == "" || s.creds == nil {
		return false, true
	}
	_, err := s.creds.Resolve(cfgSvc.CredentialRef)
	return err == nil, true
}

func (s *Server) serviceEffectiveEnabled(cfg Config, serviceID string) (bool, string) {
	if !cfg.serviceEnabled(serviceID) {
		return false, "service is disabled"
	}
	info, ok := s.registry.ServiceInfo(serviceID)
	if !ok {
		return false, "service is not registered"
	}
	configured, required := s.serviceCredentialStatus(info, cfg.Service(serviceID))
	if required && !configured {
		return false, "service credential is not configured"
	}
	return true, ""
}

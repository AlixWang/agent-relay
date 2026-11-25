package mcpgateway

import (
	"sync"
	"time"
)

// Limiter is the in-memory per-principal rate limiter (design v5 §3.4 step 6,
// §6 knobs). Windows are fixed (minute/hour); a restart resets counters, which
// is acceptable for abuse damping and documented in the design.
type Limiter struct {
	mu          sync.Mutex
	now         func() time.Time
	limits      Limits
	services    map[string]Limits
	minute      map[string]window
	hour        map[string]window
	low         map[string]window
	high        map[string]window
	unauth      map[string]window
	lastCleanup int64
}

type window struct {
	start int64
	count int
}

// Limits is the subset of gateway config the limiter needs.
type Limits struct {
	PerMinute        int
	PerHour          int
	LowWritePerHour  int
	HighWritePerHour int
}

func LimitFromConfig(cfg Config) Limits {
	return Limits{
		PerMinute:        cfg.RatePerMinutePerPeer,
		PerHour:          cfg.RatePerHourPerPeer,
		LowWritePerHour:  cfg.Write.LowRiskWritePerHour,
		HighWritePerHour: cfg.Write.HighRiskWritePerHour,
	}
}

func NewLimiter(cfg Config) *Limiter {
	l := &Limiter{
		now: time.Now, limits: LimitFromConfig(cfg),
		services: map[string]Limits{},
		minute:   map[string]window{}, hour: map[string]window{},
		low: map[string]window{}, high: map[string]window{},
		unauth: map[string]window{},
	}
	for id := range cfg.Write.Services {
		low, high := cfg.writeHourlyLimitsFor(id)
		l.services[id] = Limits{
			PerMinute: cfg.RatePerMinutePerPeer, PerHour: cfg.RatePerHourPerPeer,
			LowWritePerHour: low, HighWritePerHour: high,
		}
	}
	return l
}

func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now != nil {
		l.now = now
	}
}

// Allow checks and consumes one call (both general and, when write=true, write
// quota). Retained for direct callers/tests; the server splits general vs
// write consumption so dry-runs and approval requests don't burn write quota.
func (l *Limiter) Allow(principal, serviceID string, write bool, risk string) (bool, int, string) {
	if ok, retry, reason := l.AllowGeneral(principal); !ok {
		return false, retry, reason
	}
	if write {
		return l.AllowWrite(principal, serviceID, risk)
	}
	return true, 0, ""
}

// AllowGeneral checks and consumes the per-principal minute/hour call quotas.
func (l *Limiter) AllowGeneral(principal string) (bool, int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	minStart := now / 60 * 60
	hourStart := now / 3600 * 3600
	l.cleanupLocked(now)

	m := l.bucket(l.minute, principal, minStart)
	h := l.bucket(l.hour, principal, hourStart)
	if l.limits.PerMinute > 0 && m.count >= l.limits.PerMinute {
		return false, int(minStart + 60 - now), "rate_per_minute"
	}
	if l.limits.PerHour > 0 && h.count >= l.limits.PerHour {
		return false, int(hourStart + 3600 - now), "rate_per_hour"
	}
	m.count++
	h.count++
	l.minute[principal] = m
	l.hour[principal] = h
	return true, 0, ""
}

// AllowWrite checks and consumes the per-principal+service hourly write quota.
func (l *Limiter) AllowWrite(principal, serviceID, risk string) (bool, int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	hourStart := now / 3600 * 3600
	limits := l.limits
	if svc, ok := l.services[serviceID]; ok {
		limits = svc
	}
	l.cleanupLocked(now)

	key := principal + "\x00" + serviceID
	var wb map[string]window
	limit, reason := 0, ""
	if risk == RiskHigh {
		wb, limit, reason = l.high, limits.HighWritePerHour, "high_risk_write_per_hour"
	} else {
		wb, limit, reason = l.low, limits.LowWritePerHour, "low_risk_write_per_hour"
	}
	w := l.bucket(wb, key, hourStart)
	if limit > 0 && w.count >= limit {
		return false, int(hourStart + 3600 - now), reason
	}
	w.count++
	wb[key] = w
	return true, 0, ""
}

const maxUnauthAuditPerMinute = 5

// AllowUnauthAudit bounds DB audit rows persisted for unauthenticated requests
// from the same IP so floods cannot fill SQLite or evict real audit history.
func (l *Limiter) AllowUnauthAudit(ip string) bool {
	if ip == "" {
		ip = "unknown"
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	minStart := now / 60 * 60
	l.cleanupLocked(now)
	w := l.bucket(l.unauth, ip, minStart)
	if w.count >= maxUnauthAuditPerMinute {
		return false
	}
	w.count++
	l.unauth[ip] = w
	return true
}

func (l *Limiter) bucket(m map[string]window, key string, start int64) window {
	w := m[key]
	if w.start != start {
		w = window{start: start}
	}
	return w
}

// Cleanup drops stale windows to bound memory. Called opportunistically from
// Allow; no background goroutine is needed.
func (l *Limiter) Cleanup(now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lastCleanup = 0
	l.cleanupLocked(now)
}

func (l *Limiter) cleanupLocked(now int64) {
	if l.lastCleanup != 0 && now-l.lastCleanup < 60 {
		return
	}
	l.lastCleanup = now
	cut := now - 7200
	for _, m := range []map[string]window{l.minute, l.hour, l.low, l.high, l.unauth} {
		if len(m) < 4096 {
			continue
		}
		for k, w := range m {
			if w.start < cut {
				delete(m, k)
			}
		}
	}
}

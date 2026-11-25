package mcpgateway

import (
	"sync"
	"time"
)

// Limiter is the in-memory per-principal rate limiter (design v5 §3.4 step 6,
// §6 knobs). Windows are fixed (minute/hour); a restart resets counters, which
// is acceptable for abuse damping and documented in the design.
type Limiter struct {
	mu       sync.Mutex
	now      func() time.Time
	limits   Limits
	services map[string]Limits
	minute   map[string]window
	hour     map[string]window
	low      map[string]window
	high     map[string]window
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
	if now != nil {
		l.now = now
	}
}

// Allow checks and consumes one call. write selects the write bucket; risk is
// RiskLow or RiskHigh. It returns (allowed, retryAfterSeconds, reason).
func (l *Limiter) Allow(principal, serviceID string, write bool, risk string) (bool, int, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now().Unix()
	minStart := now / 60 * 60
	hourStart := now / 3600 * 3600
	limits := l.limits
	if svc, ok := l.services[serviceID]; ok {
		limits = svc
	}
	l.cleanupLocked(now)

	m := l.bucket(l.minute, principal, minStart)
	h := l.bucket(l.hour, principal, hourStart)
	if limits.PerMinute > 0 && m.count >= limits.PerMinute {
		return false, int(minStart + 60 - now), "rate_per_minute"
	}
	if limits.PerHour > 0 && h.count >= limits.PerHour {
		return false, int(hourStart + 3600 - now), "rate_per_hour"
	}
	var wb map[string]window
	limit, reason := 0, ""
	if write {
		if risk == RiskHigh {
			wb, limit, reason = l.high, l.limits.HighWritePerHour, "high_risk_write_per_hour"
		} else {
			wb, limit, reason = l.low, l.limits.LowWritePerHour, "low_risk_write_per_hour"
		}
		if wb != nil {
			w := l.bucket(wb, principal, hourStart)
			if limit > 0 && w.count >= limit {
				return false, int(hourStart + 3600 - now), reason
			}
			w.count++
			wb[principal] = w
		}
	}
	m.count++
	h.count++
	l.minute[principal] = m
	l.hour[principal] = h
	return true, 0, ""
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
	l.cleanupLocked(now)
}

func (l *Limiter) cleanupLocked(now int64) {
	cut := now - 7200
	for _, m := range []map[string]window{l.minute, l.hour, l.low, l.high} {
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

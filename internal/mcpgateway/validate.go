package mcpgateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
)

// Generic validation helpers shared by the core and the service packages
// (design v5 §1.4). Service-specific whitelists stay in the service packages.

// DecodeStrict decodes params and rejects unknown fields + trailing data. This
// is called for every tool that declares a struct validator, so a typo like
// "siteURL" fails instead of silently becoming an empty site.
func DecodeStrict(raw json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if len(raw) > 256<<10 {
		return InvalidParams("params are too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return InvalidParams("invalid params: %s", err.Error())
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return InvalidParams("invalid params: trailing data")
	}
	return nil
}

// NormalizeURL validates a user-supplied http(s) URL and strips the fragment.
// It rejects userinfo and non-http schemes; no request is made here.
func NormalizeURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("url is required")
	}
	if len(raw) > 2048 {
		return nil, fmt.Errorf("url is too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("url is not valid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url must be http(s)")
	}
	if u.User != nil {
		return nil, fmt.Errorf("url must not contain credentials")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("url has no host")
	}
	u.Fragment = ""
	return u, nil
}

// URLBelongsToSite checks that target shares site's host and stays under the
// site's base path (design v5 §1.4: URL must belong to a whitelisted site).
func URLBelongsToSite(site, target string) bool {
	s, err := NormalizeURL(site)
	if err != nil {
		return false
	}
	t, err := NormalizeURL(target)
	if err != nil {
		return false
	}
	if !strings.EqualFold(s.Host, t.Host) {
		return false
	}
	base := strings.TrimSuffix(s.EscapedPath(), "/")
	if base == "" {
		return true
	}
	p := t.EscapedPath()
	return p == base || strings.HasPrefix(p, base+"/")
}

// ValidateSiteURL checks a site identifier (the exact string from list_sites)
// and returns the parsed form.
func ValidateSiteURL(raw string) (*url.URL, error) {
	u, err := NormalizeURL(raw)
	if err != nil {
		return nil, fmt.Errorf("site: %w", err)
	}
	return u, nil
}

// ParseDateLike accepts YYYY-MM-DD or RFC3339 and returns the canonical date
// (YYYY-MM-DD). AdSense and Bing both speak date-only upstream parameters.
func ParseDateLike(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("date is required")
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.Format("2006-01-02"), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.Format("2006-01-02"), nil
	}
	return "", fmt.Errorf("date %q must be YYYY-MM-DD or RFC3339", raw)
}

// ValidateDateRange parses and orders a start/end pair.
func ValidateDateRange(start, end string) (string, string, error) {
	s, err := ParseDateLike(start)
	if err != nil {
		return "", "", fmt.Errorf("start_date: %w", err)
	}
	e, err := ParseDateLike(end)
	if err != nil {
		return "", "", fmt.Errorf("end_date: %w", err)
	}
	if e < s {
		return "", "", fmt.Errorf("end_date must be on or after start_date")
	}
	return s, e, nil
}

// DedupeStrings removes exact duplicates while preserving order.
func DedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ValidateBatchURLs enforces batch cap + URL ownership and returns the
// de-duplicated list (design v5: cap 100, upstream allows 500).
func ValidateBatchURLs(site string, urls []string, cap int) ([]string, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("urls is required")
	}
	if cap <= 0 {
		cap = 100
	}
	clean := make([]string, 0, len(urls))
	for i, raw := range urls {
		u, err := NormalizeURL(raw)
		if err != nil {
			return nil, fmt.Errorf("urls[%d]: %w", i, err)
		}
		if !URLBelongsToSite(site, u.String()) {
			return nil, fmt.Errorf("urls[%d] does not belong to site", i)
		}
		clean = append(clean, u.String())
	}
	clean = DedupeStrings(clean)
	if len(clean) > cap {
		return nil, fmt.Errorf("batch has %d URLs; cap is %d", len(clean), cap)
	}
	return clean, nil
}

// ValidateLimit clamps an optional limit to the configured max rows.
func ValidateLimit(limit *int, def, max int) int {
	if max <= 0 {
		max = 1000
	}
	if limit == nil || *limit <= 0 {
		if def <= 0 || def > max {
			return max
		}
		return def
	}
	if *limit > max {
		return max
	}
	return *limit
}

var secretKeyNames = map[string]bool{
	"authorization": true, "cookie": true, "password": true, "secret": true,
	"token": true, "access_token": true, "refresh_token": true, "apikey": true,
	"api_key": true, "key": true, "credentials": true, "client_secret": true,
}

// ParamsSummary returns a short, credential-free summary for audit rows. Key
// names that could carry credentials are replaced, long strings truncated,
// and the whole thing capped at 500 bytes. The raw params are never logged.
func ParamsSummary(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return "invalid-json"
	}
	var b strings.Builder
	writeSummary(&b, v, 0)
	out := b.String()
	if len(out) > 500 {
		out = out[:500] + "…"
	}
	return out
}

func writeSummary(b *strings.Builder, v any, depth int) {
	if depth > 3 {
		b.WriteString("…")
		return
	}
	switch x := v.(type) {
	case map[string]any:
		b.WriteByte('{')
		first := true
		for k, val := range x {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString(k)
			b.WriteByte(':')
			if secretKeyNames[strings.ToLower(k)] {
				b.WriteString("***")
			} else {
				writeSummary(b, val, depth+1)
			}
			if b.Len() > 500 {
				return
			}
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, val := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeSummary(b, val, depth+1)
			if b.Len() > 500 {
				return
			}
		}
		b.WriteByte(']')
	case string:
		if u, err := url.Parse(x); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			// Query strings are a common place for leaked tokens; audit keeps
			// the site/page identity only.
			u.RawQuery = ""
			u.Fragment = ""
			u.User = nil
			x = u.String()
		}
		if len(x) > 120 {
			x = x[:120] + "…"
		}
		b.WriteString(x)
	case json.Number:
		b.WriteString(x.String())
	default:
		fmt.Fprintf(b, "%v", x)
	}
}

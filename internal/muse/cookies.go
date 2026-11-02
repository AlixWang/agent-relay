package muse

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Only hatch_sess is required at import time: it is the login session. The
// short-lived hatch_vml VM credential is issued/rotated by GET /api/session and
// is absent from a fresh browser session, so requiring it would reject a valid
// cookie (muse2api's ESSENTIAL_COOKIES is likewise hatch_sess only).
var essentialNames = []string{"hatch_sess"}

// expiryNames feed Account.ExpiresAt. hatch_vml is optional at import but is
// the tighter deadline once /api/session has issued it.
var expiryNames = []string{"hatch_sess", "hatch_vml"}

const (
	maxCookieInput = 32 << 10
	maxCookies     = 64
)

// parseCookieInput accepts a browser Cookie header ("a=b; c=d") or a JSON
// object. Only names/values are kept; the caller encrypts them.
func parseCookieInput(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty cookie string")
	}
	if len(raw) > maxCookieInput {
		return nil, fmt.Errorf("cookie string too long (max %d bytes)", maxCookieInput)
	}
	if strings.HasPrefix(raw, "{") {
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("invalid cookie JSON")
		}
		if err := validateCookieMap(m); err != nil {
			return nil, err
		}
		return m, nil
	}
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "cookie:") {
		raw = strings.TrimSpace(raw[7:])
	}
	m := map[string]string{}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if !ok || name == "" {
			return nil, fmt.Errorf("cookie %q is not name=value", part)
		}
		if !validCookieName(name) {
			return nil, fmt.Errorf("invalid cookie name %q", name)
		}
		if hasControl(value) || strings.Contains(value, ";") {
			return nil, fmt.Errorf("invalid value for cookie %q", name)
		}
		m[name] = value
		if len(m) > maxCookies {
			return nil, fmt.Errorf("too many cookies (max %d)", maxCookies)
		}
	}
	if err := validateCookieMap(m); err != nil {
		return nil, err
	}
	return m, nil
}

func validateCookieMap(m map[string]string) error {
	if len(m) == 0 {
		return fmt.Errorf("no cookies parsed")
	}
	if len(m) > maxCookies {
		return fmt.Errorf("too many cookies (max %d)", maxCookies)
	}
	for name, value := range m {
		if !validCookieName(name) {
			return fmt.Errorf("invalid cookie name %q", name)
		}
		if hasControl(value) || strings.Contains(value, ";") {
			return fmt.Errorf("invalid value for cookie %q", name)
		}
	}
	return nil
}

// validCookieName implements the RFC 6265 token grammar closely enough for
// browser cookies (no separators/whitespace/control characters).
func validCookieName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c)):
		default:
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

func missingEssential(cookies map[string]string) []string {
	var missing []string
	for _, name := range essentialNames {
		if strings.TrimSpace(cookies[name]) == "" {
			missing = append(missing, name)
		}
	}
	return missing
}

func cookieHeader(cookies map[string]string) string {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, name+"="+cookies[name])
	}
	return strings.Join(parts, "; ")
}

// mergeSetCookies applies muse.ai's rotation response. MaxAge<0 deletes;
// otherwise value/expiry are updated when present. A response without any
// expiry keeps the previous one (never fabricate a lifetime).
func mergeSetCookies(cookies map[string]string, exp map[string]int64, set []*http.Cookie, now int64) {
	if cookies == nil {
		return
	}
	if exp == nil {
		exp = map[string]int64{}
	}
	for _, c := range set {
		if c == nil || c.Name == "" {
			continue
		}
		// Max-Age<0 and the classic "empty value + past Expires" both delete.
		if c.MaxAge < 0 || (c.MaxAge == 0 && !c.Expires.IsZero() && c.Expires.Before(time.Unix(now, 0))) {
			delete(cookies, c.Name)
			delete(exp, c.Name)
			continue
		}
		if c.Value != "" {
			cookies[c.Name] = c.Value
		}
		switch {
		case c.MaxAge > 0:
			exp[c.Name] = now + int64(c.MaxAge)
		case !c.Expires.IsZero():
			exp[c.Name] = c.Expires.Unix()
		}
	}
}

func minEssentialExpiry(exp map[string]int64) int64 {
	var min int64
	for _, name := range expiryNames {
		v := exp[name]
		if v > 0 && (min == 0 || v < min) {
			min = v
		}
	}
	return min
}

func marshalCookies(cookies map[string]string) ([]byte, error) {
	if cookies == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(cookies)
}

func parseCookiesExp(raw string) map[string]int64 {
	out := map[string]int64{}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	_ = json.Unmarshal([]byte(raw), &out)
	return out
}

func marshalCookiesExp(exp map[string]int64) string {
	if len(exp) == 0 {
		return "{}"
	}
	raw, err := json.Marshal(exp)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func sortedCookieNames(cookies map[string]string) []string {
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

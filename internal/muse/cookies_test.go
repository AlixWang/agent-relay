package muse

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestParseCookieInput(t *testing.T) {
	m, err := parseCookieInput("Cookie: hatch_sess=s; hatch_vml=v; hatch_gw=g; empty=")
	if err != nil {
		t.Fatal(err)
	}
	if m["hatch_sess"] != "s" || m["hatch_vml"] != "v" || m["hatch_gw"] != "g" || m["empty"] != "" {
		t.Fatalf("parsed: %+v", m)
	}
	if missing := missingEssential(m); len(missing) != 0 {
		t.Fatalf("missing: %v", missing)
	}

	j, err := parseCookieInput(`{"hatch_sess":"s","hatch_vml":"v"}`)
	if err != nil || j["hatch_sess"] != "s" {
		t.Fatalf("json: %+v (%v)", j, err)
	}

	if one, err := parseCookieInput("hatch_sess=s"); err != nil {
		t.Fatalf("parse one: %v", err)
	} else if missing := missingEssential(one); len(missing) != 1 || missing[0] != "hatch_vml" {
		t.Fatalf("missing detection: %v", missing)
	}
	if _, err := parseCookieInput("bad name=x"); err == nil {
		t.Fatal("bad name accepted")
	}
	if _, err := parseCookieInput("a=b\x01"); err == nil {
		t.Fatal("control char accepted")
	}
	if _, err := parseCookieInput(`{"hatch_sess":"a;b","hatch_vml":"v"}`); err == nil {
		t.Fatal("semicolon in JSON value accepted")
	}
	var many strings.Builder
	for i := 0; i <= maxCookies; i++ {
		many.WriteString("c")
		many.WriteString(string(rune('A' + i%26)))
		many.WriteString(string(rune('a' + i/26)))
		many.WriteString("=v;")
	}
	if _, err := parseCookieInput(many.String()); err == nil {
		t.Fatal("too many cookies accepted")
	}
	if _, err := parseCookieInput(strings.Repeat("a", maxCookieInput+1)); err == nil {
		t.Fatal("oversized cookie string accepted")
	}
}

func TestCookieHeaderSorted(t *testing.T) {
	got := cookieHeader(map[string]string{"b": "2", "a": "1", "c": "3"})
	if got != "a=1; b=2; c=3" {
		t.Fatalf("header: %q", got)
	}
}

func TestMergeSetCookies(t *testing.T) {
	now := int64(1000)
	cookies := map[string]string{"hatch_sess": "old", "hatch_vml": "old-vml"}
	exp := map[string]int64{"hatch_vml": 2000}
	expires := time.Unix(3000, 0)
	set := []*http.Cookie{
		{Name: "hatch_sess", Value: "new", MaxAge: 50},
		{Name: "hatch_vml", Value: "new-vml", Expires: expires},
		{Name: "hatch_gw", Value: "gw", MaxAge: 10},
		{Name: "gone", Value: "x", MaxAge: -1},
	}
	cookies["gone"] = "x"
	exp["gone"] = 1
	mergeSetCookies(cookies, exp, set, now)
	if cookies["hatch_sess"] != "new" || exp["hatch_sess"] != now+50 {
		t.Fatalf("max-age: %+v %+v", cookies, exp)
	}
	if cookies["hatch_vml"] != "new-vml" || exp["hatch_vml"] != 3000 {
		t.Fatalf("expires: %+v %+v", cookies, exp)
	}
	if cookies["hatch_gw"] != "gw" || exp["hatch_gw"] != now+10 {
		t.Fatalf("new cookie: %+v %+v", cookies, exp)
	}
	if _, ok := cookies["gone"]; ok {
		t.Fatalf("max-age<0 did not delete: %+v", cookies)
	}
	if exp["hatch_vml"] != 3000 || exp["hatch_sess"] != now+50 {
		t.Fatalf("expiry: %+v", exp)
	}

	// No expiry in the response keeps the previous value.
	mergeSetCookies(cookies, exp, []*http.Cookie{{Name: "hatch_vml", Value: "again"}}, now)
	if exp["hatch_vml"] != 3000 {
		t.Fatalf("session cookie clobbered expiry: %+v", exp)
	}

	// The classic deletion form (empty value + past Expires) must remove.
	cookies["expired"] = "old"
	exp["expired"] = 1
	mergeSetCookies(cookies, exp, []*http.Cookie{
		{Name: "expired", Value: "", Expires: time.Unix(500, 0)},
	}, now)
	if _, ok := cookies["expired"]; ok {
		t.Fatalf("past Expires did not delete: %+v", cookies)
	}
}

func TestMinEssentialExpiry(t *testing.T) {
	if got := minEssentialExpiry(map[string]int64{"hatch_sess": 5000, "hatch_vml": 3000}); got != 3000 {
		t.Fatalf("min: %d", got)
	}
	if got := minEssentialExpiry(map[string]int64{"hatch_sess": 5000}); got != 5000 {
		t.Fatalf("single: %d", got)
	}
	if got := minEssentialExpiry(nil); got != 0 {
		t.Fatalf("empty: %d", got)
	}
}

func TestCookiesExpJSON(t *testing.T) {
	raw := marshalCookiesExp(map[string]int64{"hatch_vml": 123})
	if raw != `{"hatch_vml":123}` {
		t.Fatalf("marshal: %s", raw)
	}
	got := parseCookiesExp(raw)
	if got["hatch_vml"] != 123 {
		t.Fatalf("parse: %+v", got)
	}
	if parseCookiesExp("not-json") == nil {
		t.Fatal("malformed expiry must return an empty map")
	}
	if marshalCookiesExp(nil) != "{}" {
		t.Fatal("nil expiry must marshal to {}")
	}
}

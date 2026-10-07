package gateway

import (
	"os"
	"testing"
	"time"
)

func TestVersionCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.3.0", "v0.2.0", 1},
		{"v0.2.0", "v0.3.0", -1},
		{"v1.0.0", "v1.0.0", 0},
		{"v0.10.0", "v0.9.0", 1},
		{"v2.0", "v10.0", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Fatalf("compare %s %s: got %d want %d", c.a, c.b, got, c.want)
		}
	}
	if !updateNewer("v0.2.0", "v0.3.0") || updateNewer("v0.3.0", "v0.2.0") || updateNewer("v0.3.0", "v0.3.0") {
		t.Fatal("updateNewer wrong")
	}
}

func TestUpdateStatusShape(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/update/status", nil, admin)
	if c != 200 {
		t.Fatalf("status: %d %+v", c, out)
	}
	cur, _ := out["current"].(map[string]any)
	if cur["protocol"] == nil || cur["prompt_version"] == nil {
		t.Fatalf("current shape: %+v", out)
	}
	if _, ok := out["mode"]; !ok {
		t.Fatalf("mode missing: %+v", out)
	}
}

func TestUpdateApplyGuards(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	// Bad version format.
	c, _ := f.doAuth(t, "POST", "/admin/update/apply",
		map[string]any{"version": "main"}, admin)
	if c != 400 {
		t.Fatalf("bad version should 400, got %d", c)
	}
	// Non-systemd mode refuses (tests run as unknown/docker-less env;
	// updateMode() returns unknown here, never systemd).
	c, out := f.doAuth(t, "POST", "/admin/update/apply",
		map[string]any{"version": "v9.9.9", "acknowledge_protocol_change": true}, admin)
	if c != 400 || !containsStr(out["error"], "systemd") {
		t.Fatalf("non-systemd should 400 systemd-only, got %d %+v", c, out)
	}
	// Unauthenticated.
	c, _ = f.do(t, "POST", "/admin/update/apply", map[string]any{"version": "v9.9.9"}, "")
	if c != 401 {
		t.Fatalf("unauth should 401, got %d", c)
	}
	// Downgrade guard: BinaryVersion is dev in tests → skipped, so check
	// the pure function instead.
	if updateNewer("v9.9.9", "v0.1.0") {
		t.Fatal("downgrade must not be newer")
	}
}

func containsStr(v any, sub string) bool {
	s, _ := v.(string)
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestUpdateCheckResolvesLatest(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	// Empty version → latest lookup. With network it resolves (dev build
	// skips the downgrade guard); offline it 502s. Either way it must NOT
	// 400 on validation — empty is a valid "give me latest" request.
	c, out := f.doAuth(t, "POST", "/admin/update/check",
		map[string]any{"version": ""}, admin)
	if c == 400 {
		t.Fatalf("empty version must not 400: %+v", out)
	}
	if c == 200 && out["version"] == nil {
		t.Fatalf("resolved check missing version: %+v", out)
	}
	// Bad shape still 400.
	c, _ = f.doAuth(t, "POST", "/admin/update/check",
		map[string]any{"version": "main"}, admin)
	if c != 400 {
		t.Fatalf("bad version should 400, got %d", c)
	}
}

func TestUpdateJobMutex(t *testing.T) {
	mgr := newUpdateManager()
	j1, ok := mgr.start("v9.9.9")
	if !ok || j1 == nil {
		t.Fatal("first start should win")
	}
	if _, ok := mgr.start("v9.9.10"); ok {
		t.Fatal("second start must 409-busy")
	}
	mgr.finish(j1.ID, "failed", "x", 1)
	if _, ok := mgr.start("v9.9.10"); !ok {
		t.Fatal("after finish should accept")
	}
}

func TestPendingTriggerWritten(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.srv.cfg.DataDir = dir
	mgr := f.srv.updateMgr()
	job, ok := mgr.start("v9.9.9")
	if !ok {
		t.Fatal("start")
	}
	done := make(chan struct{})
	go func() { f.srv.runUpdateJob(job); close(done) }()
	// The .req file must appear quickly (no sudo, just a file write).
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(dir + "/update-jobs/pending/v9.9.9.req"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending .req not written")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Simulate the root timer completing the job.
	os.WriteFile(dir+"/update-jobs/v9.9.9.log", []byte("UPDATE_RESULT ok v9.9.9\n"), 0o644)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runUpdateJob did not finish")
	}
	if j := mgr.get(job.ID); j.Status != "ok" {
		t.Fatalf("job: %+v", j)
	}
}

package gateway

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
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

// The helper restarts the gateway in the middle of an update, so the console's
// view of a job must come from disk. Regression test for the reported bug: a
// successful update left the panel on "running" forever (the restarted process
// had never heard of the job) and the outcome never reached the audit log.
func TestUpdateJobSurvivesRestart(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	f.srv.cfg.DataDir = dir

	rec, busy, err := f.srv.startUpdateJob("v9.9.9")
	if busy != "" || err != nil {
		t.Fatalf("start job: busy=%q err=%v", busy, err)
	}
	// In-flight state is on disk: a second request is refused even though
	// nothing about this job lives in memory.
	if _, busy2, _ := f.srv.startUpdateJob("v9.9.10"); busy2 == "" {
		t.Fatal("second job must be refused while one is in flight")
	}
	if _, err := os.Stat(f.srv.jobReqPath("v9.9.9")); err != nil {
		t.Fatalf("timer trigger missing: %v", err)
	}

	// The timer picked it up and the helper started logging.
	if err := os.WriteFile(f.srv.jobLogPath("v9.9.9"),
		[]byte("[update] target v9.9.9 arch amd64\n[update] checksum ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(f.srv.jobReqPath("v9.9.9"))
	j := f.srv.currentUpdateJob()
	if j == nil || j.Status != "running" {
		t.Fatalf("live view: %+v", j)
	}
	if !strings.Contains(j.Log, "checksum ok") {
		t.Fatalf("live log must stream the helper output, got %q", j.Log)
	}
	if len(j.Phases) != 5 || j.Phases[1].Status != "done" || j.Phases[2].Status != "active" {
		t.Fatalf("phases: %+v", j.Phases)
	}

	// Restart: the process that accepted the job is gone. A fresh server
	// reading the same data dir must still see the job and its live log.
	f2 := newFixture(t)
	f2.srv.cfg.DataDir = dir
	admin := f2.adminLogin(t)
	c, out := f2.doAuth(t, "GET", "/admin/update/status", nil, admin)
	if c != 200 {
		t.Fatalf("status after restart: %d %+v", c, out)
	}
	job, _ := out["job"].(map[string]any)
	if job == nil || job["status"] != "running" || job["id"] != rec.ID {
		t.Fatalf("job after restart: %+v", out)
	}
	// ...and must still refuse a second concurrent job.
	if _, busy3, _ := f2.srv.startUpdateJob("v9.9.10"); busy3 == "" {
		t.Fatal("restarted process must still see the in-flight job")
	}

	// The helper finishes while the process is restarting.
	if err := os.WriteFile(f2.srv.jobLogPath("v9.9.9"),
		[]byte("[update] target v9.9.9 arch amd64\n[update] checksum ok\n"+
			"[update] installed, restarting\n[update] health ok\nUPDATE_RESULT ok v9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f2.srv.ReconcileUpdateJobs()
	c, out = f2.doAuth(t, "GET", "/admin/update/status", nil, admin)
	if c != 200 {
		t.Fatalf("status: %d", c)
	}
	job, _ = out["job"].(map[string]any)
	if job == nil || job["status"] != "ok" {
		t.Fatalf("finished job: %+v", job)
	}
	if phases, _ := job["phases"].([]any); len(phases) != 5 {
		t.Fatalf("finished job phases: %+v", job["phases"])
	}
	// The audit entry is written exactly once, by whichever process notices.
	entries, _, err := f2.srv.st.ListAuditPage(store.AuditFilter{}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	okEntries := 0
	for _, e := range entries {
		if e.Action == "update.ok" {
			okEntries++
		}
	}
	if okEntries != 1 {
		t.Fatalf("want exactly 1 update.ok audit entry, got %d", okEntries)
	}
	f2.srv.ReconcileUpdateJobs() // idempotent
	entries, _, _ = f2.srv.st.ListAuditPage(store.AuditFilter{}, 50, 0)
	okEntries = 0
	for _, e := range entries {
		if e.Action == "update.ok" {
			okEntries++
		}
	}
	if okEntries != 1 {
		t.Fatalf("reconcile must not duplicate audit entries, got %d", okEntries)
	}
	// And a new update is allowed again.
	if _, busy4, err := f2.srv.startUpdateJob("v9.9.11"); busy4 != "" || err != nil {
		t.Fatalf("must allow a new job once the last one finished: %q %v", busy4, err)
	}
}

// The panel must never claim "running" forever: when the running binary is
// already the target, the update landed even if the helper's result line was
// lost with the restart.
func TestUpdateJobViewInference(t *testing.T) {
	now := time.Now()
	rec := jobRecord{ID: "upd-1", Version: "v0.15.4", StartedAt: now.Add(-2 * time.Minute).Unix()}
	mid := helperLog{
		Mark:    map[string]string{"queue": "done", "download": "done", "install": "done", "restart": "active"},
		Lines:   []string{"[update] target v0.15.4 arch amd64"},
		ModTime: now.Add(-20 * time.Second),
	}

	v := updateJobView(rec, mid, true, false, "v0.15.4", now)
	if v.Status != "ok" || !v.Confirmed {
		t.Fatalf("target already running must read as ok: %+v", v)
	}
	if v2 := updateJobView(rec, mid, true, false, "v0.15.3", now); v2.Status != "running" || v2.Confirmed {
		t.Fatalf("old binary still running must read as running: %+v", v2)
	}
	// Exactly one phase is "active" at a time: a started phase must not also
	// light up its successor (seen in the live run as "downloading" plus
	// "installing" at once).
	working := updateJobView(rec, helperLog{
		Mark:  map[string]string{"queue": "done", "download": "active"},
		Lines: []string{"x"}, ModTime: now,
	}, true, false, "v0.15.3", now)
	active := 0
	for _, p := range working.Phases {
		if p.Status == "active" {
			active++
		}
	}
	if active != 1 || working.Phases[1].Status != "active" || working.Phases[2].Status != "pending" {
		t.Fatalf("phases must have exactly one active row: %d %+v", active, working.Phases)
	}

	// queued too long → point at the timer instead of waiting forever
	q := updateJobView(jobRecord{ID: "j2", Version: "v0.15.4", StartedAt: now.Add(-5 * time.Minute).Unix()},
		helperLog{}, false, true, "v0.15.3", now)
	if q.Status != "queued" || !strings.Contains(q.Hint, "agent-relay-update.timer") {
		t.Fatalf("stale queue must hint at the timer: %+v", q)
	}
	if q.Phases[0].Status != "active" {
		t.Fatalf("queued job must show the first phase active: %+v", q.Phases)
	}
	// stuck helper → hint at journalctl
	stuck := updateJobView(rec, helperLog{Mark: map[string]string{"queue": "done"}, Lines: []string{"x"}, ModTime: now.Add(-11 * time.Minute)}, true, false, "v0.15.3", now)
	if stuck.Status != "running" || !strings.Contains(stuck.Hint, "journalctl") {
		t.Fatalf("stale log must hint at journalctl: %+v", stuck)
	}
	// finished → every phase done, elapsed measured from the request
	done := updateJobView(rec, helperLog{Result: "ok", Detail: "v0.15.4", Mark: map[string]string{}, Lines: []string{"x"}, ModTime: now}, true, false, "v0.15.4", now)
	if !done.Confirmed || done.Elapsed != 120 {
		t.Fatalf("finished view: %+v", done)
	}
	for _, p := range done.Phases {
		if p.Status != "done" {
			t.Fatalf("finished job phase %s: %s", p.Name, p.Status)
		}
	}
	// rolled back → not ok, earlier phases keep their marks
	rb := updateJobView(rec, helperLog{Result: "rolled_back", Detail: "health-failed",
		Mark:  map[string]string{"queue": "done", "download": "done", "install": "done", "restart": "done", "health": "failed"},
		Lines: []string{"x"}, ModTime: now}, true, false, "v0.15.3", now)
	if rb.Status != "rolled_back" || rb.Phases[4].Status != "failed" || rb.Phases[2].Status != "done" {
		t.Fatalf("rolled back view: %+v %+v", rb, rb.Phases)
	}
}

// Both helper generations must produce the same checklist, so the console
// shows progress on servers whose helper predates the STEP protocol.
func TestHelperLogParsing(t *testing.T) {
	dir := t.TempDir()
	legacy := dir + "/legacy.log"
	if err := os.WriteFile(legacy, []byte(
		"[update] target v9.9.9 arch amd64\n"+
			"[update] checksum ok\n"+
			"[update] installed, restarting\n"+
			"[update] health ok\n"+
			"UPDATE_RESULT ok v9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hl, ok := parseHelperLog(legacy)
	if !ok || hl.Result != "ok" {
		t.Fatalf("legacy result: %+v ok=%v", hl, ok)
	}
	for _, phase := range []string{"queue", "download", "install", "restart", "health"} {
		if hl.Mark[phase] != "done" {
			t.Fatalf("legacy phase %s: %q", phase, hl.Mark[phase])
		}
	}
	modern := dir + "/modern.log"
	if err := os.WriteFile(modern, []byte(
		"10:00:01 STEP queue picked up v9.9.9 (job upd-1)\n"+
			"10:00:02 STEP download GET release url\n"+
			"10:00:09 STEP checksum ok deadbeef\n"+
			"10:00:10 [update] installed, restarting\n"+
			"10:00:14 STEP health ok\n"+
			"10:00:14 UPDATE_RESULT ok v9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hl2, _ := parseHelperLog(modern)
	if hl2.Result != "ok" || hl2.Mark["download"] != "done" || hl2.Mark["health"] != "done" {
		t.Fatalf("modern: %+v", hl2)
	}
	if !strings.Contains(hl2.PhaseDet["queue"], "upd-1") {
		t.Fatalf("step detail lost: %+v", hl2.PhaseDet)
	}
	failed := dir + "/failed.log"
	if err := os.WriteFile(failed, []byte(
		"STEP download GET url\nSTEP checksum mismatch expected=a actual=b\n"+
			"UPDATE_RESULT failed checksum\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hl3, _ := parseHelperLog(failed)
	if hl3.Mark["download"] != "failed" || hl3.Mark["install"] != "" {
		t.Fatalf("failure marks: %+v", hl3.Mark)
	}
	// No log yet → no view, not a crash.
	if _, ok := parseHelperLog(dir + "/missing.log"); ok {
		t.Fatal("missing log must report ok=false")
	}
}

// The update worker (cmd/agent-relay/update-helper.sh, embedded in the binary)
// and this package's log parser are two halves of one protocol. Drift between
// them is invisible until an operator is staring at a stalled panel, so pin it:
// every phase the worker emits must be understood here, and the terminal line
// it writes must be the one this parser scans for.
func TestUpdateHelperProtocolContract(t *testing.T) {
	raw, err := os.ReadFile("../../cmd/agent-relay/update-helper.sh")
	if err != nil {
		t.Fatalf("read helper: %v", err)
	}
	script := string(raw)
	for _, need := range []string{updateResultMark, " STEP ", "--print-update-helper", "systemctl restart agent-relay", "SHA256SUMS"} {
		if !strings.Contains(script, need) {
			t.Fatalf("worker no longer contains %q: the console protocol broke", need)
		}
	}
	// Collect `step <phase> ...` / `result <state>` invocations.
	phaseRe := regexp.MustCompile(`(?m)^[ \t]*step[ \t]+([a-z][a-z-]*)`)
	seen := map[string]bool{}
	for _, m := range phaseRe.FindAllStringSubmatch(script, -1) {
		seen[m[1]] = true
	}
	if len(seen) < 6 {
		t.Fatalf("expected the worker to report several phases, got %v", seen)
	}
	// Feed each phase through the parser as if the worker had logged it.
	var lines []string
	for phase := range seen {
		lines = append(lines, "12:00:00 STEP "+phase+" detail-"+phase)
	}
	lines = append(lines, "12:00:01 "+updateResultMark+"ok v9.9.9")
	logPath := t.TempDir() + "/contract.log"
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hl, ok := parseHelperLog(logPath)
	if !ok || hl.Result != "ok" {
		t.Fatalf("parser rejected the worker log: %+v ok=%v", hl, ok)
	}
	// Every phase the worker emits must light up its checklist row, and an
	// `ok` result must leave no row unfinished.
	j := updateJobView(jobRecord{ID: "upd-x", Version: "v9.9.9", StartedAt: time.Now().Add(-time.Minute).Unix()},
		hl, true, false, "v9.9.9", time.Now())
	for _, p := range j.Phases {
		if p.Status != "done" {
			t.Fatalf("phase %q not done for a successful job: %+v", p.Name, j.Phases)
		}
	}
	unknown := []string{}
	for phase := range seen {
		switch phase {
		case "queue", "download", "checksum", "backup", "install", "helper", "restart", "health", "result":
		default:
			unknown = append(unknown, phase)
		}
	}
	if len(unknown) > 0 {
		t.Fatalf("worker emits phases the gateway does not understand: %v", unknown)
	}
}

// Transition case, and it is the release that introduces this feature: a server
// updating FROM a version that kept job state in memory has no job record, only
// the helper log. Without a fallback the console would show nothing at all
// right after the update it just performed.
func TestUpdateStatusFallsBackToHelperLog(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.DataDir = t.TempDir()
	admin := f.adminLogin(t)

	// A log from an older install: no jobs/<ver>.json anywhere.
	logPath := f.srv.jobLogPath("v0.15.5")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte(
		"[update] target v0.15.5 arch amd64\n[update] checksum ok\n"+
			"[update] installed, restarting\n[update] health ok\nUPDATE_RESULT ok v0.15.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, out := f.doAuth(t, "GET", "/admin/update/status", nil, admin)
	if c != 200 {
		t.Fatalf("status: %d", c)
	}
	job, _ := out["job"].(map[string]any)
	if job == nil || job["status"] != "ok" || job["version"] != "v0.15.5" {
		t.Fatalf("log-only job must be reported: %+v", out)
	}
	if phases, _ := job["phases"].([]any); len(phases) != 5 {
		t.Fatalf("log-only phases: %+v", job["phases"])
	}
	// A finished job does not block the next one.
	if _, busy, err := f.srv.startUpdateJob("v0.15.6"); busy != "" || err != nil {
		t.Fatalf("finished log-only job must not block: %q %v", busy, err)
	}

	// An in-flight log-only job (the update currently running, whose requesting
	// process is about to be restarted away) must still block a second one.
	f2 := newFixture(t)
	f2.srv.cfg.DataDir = t.TempDir()
	live := f2.srv.jobLogPath("v0.15.5")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(live, []byte("[update] target v0.15.5 arch amd64\n[update] checksum ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	admin2 := f2.adminLogin(t)
	_, out2 := f2.doAuth(t, "GET", "/admin/update/status", nil, admin2)
	job2, _ := out2["job"].(map[string]any)
	if job2 == nil || job2["status"] != "running" {
		t.Fatalf("in-flight log-only job: %+v", out2)
	}
	if _, busy, _ := f2.srv.startUpdateJob("v0.15.6"); busy == "" {
		t.Fatal("an in-flight log-only job must refuse a second update")
	}
	// Booting the new binary over such a log books the outcome exactly once.
	f2.srv.ReconcileUpdateJobs()
	f2.srv.ReconcileUpdateJobs()
	entries, _, _ := f2.srv.st.ListAuditPage(store.AuditFilter{}, 50, 0)
	seen := 0
	for _, e := range entries {
		if e.Action == "update.ok" || e.Action == "update.failed" || e.Action == "update.rolled_back" {
			seen++
		}
	}
	if seen != 0 {
		t.Fatalf("a still-running job must not be booked as finished, got %d entries", seen)
	}
	if err := os.WriteFile(live, []byte("UPDATE_RESULT ok v0.15.5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f2.srv.ReconcileUpdateJobs()
	f2.srv.ReconcileUpdateJobs() // idempotent: the stub record carries audited=true
	entries, _, _ = f2.srv.st.ListAuditPage(store.AuditFilter{}, 50, 0)
	seen = 0
	for _, e := range entries {
		if e.Action == "update.ok" {
			seen++
		}
	}
	if seen != 1 {
		t.Fatalf("want exactly one recovered audit entry, got %d", seen)
	}
}

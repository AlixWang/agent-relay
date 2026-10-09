package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/web"
)

// Update mode detection: docker containers carry /.dockerenv; the binary
// path is baked by install.sh/systemd vs ENTRYPOINT. AGENT_RELAY_UPDATE_MODE
// overrides the guess for non-standard layouts (systemd install outside
// /usr/local/bin, and the flow's own end-to-end test).
func updateMode() string {
	if m := os.Getenv("AGENT_RELAY_UPDATE_MODE"); m == "systemd" || m == "docker" || m == "unknown" {
		return m
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if exe, err := os.Executable(); err == nil && strings.HasPrefix(exe, "/usr/local/bin/") {
		return "systemd"
	}
	return "unknown"
}

var verRe = regexp.MustCompile(`^v[0-9][A-Za-z0-9._-]*$`)

// Update job state is disk-backed on purpose. The helper restarts this very
// process in the middle of a job, so in-memory state cannot be the source of
// truth: the console used to sit on "running" forever after a successful
// update, because the process that answered the next poll had never heard of
// the job and the browser kept rendering the last status it had seen. What
// lives where (all under data_dir/update-jobs):
//
//	jobs/<ver>.json     one record per requested job (id, version, started_at)
//	pending/<ver>.req   trigger for the root timer (content: job id)
//	<ver>.log           helper log; terminal line: UPDATE_RESULT <st> <detail>
//
// Everything the status endpoint reports is derived from those files plus the
// running binary version, so it answers correctly before, during and after the
// restart — and when the process that accepted the job is gone.

type updatePhase struct {
	Name   string `json:"name"`
	Status string `json:"status"` // pending|active|done|failed
	Detail string `json:"detail,omitempty"`
}

type updateJob struct {
	ID        string        `json:"id"`
	Version   string        `json:"version"`
	Status    string        `json:"status"` // queued|running|ok|rolled_back|failed
	Detail    string        `json:"detail,omitempty"`
	Log       string        `json:"log"`
	Phases    []updatePhase `json:"phases"`
	Hint      string        `json:"hint,omitempty"`
	StartedAt int64         `json:"started_at"`
	EndedAt   int64         `json:"ended_at"`
	Elapsed   int64         `json:"elapsed_secs"`
	Confirmed bool          `json:"confirmed"` // the running binary is already the target
}

// jobRecord is the on-disk request: it carries a job across the restart the
// helper performs, and marks whether the outcome reached the audit log
// (whoever notices first writes it exactly once).
type jobRecord struct {
	ID        string `json:"id"`
	Version   string `json:"version"`
	StartedAt int64  `json:"started_at"`
	Audited   bool   `json:"audited"`
	Status    string `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

const (
	// Prefixes the helper log is scanned for (both are matched anywhere in
	// the line: the helper prefixes lines with a timestamp).
	updateResultMark = "UPDATE_RESULT "
	updateStepMark   = " STEP "
	// The root timer polls every 30s: a queued job older than this has a
	// trigger problem worth telling the operator about.
	updateQueueGrace = 90 * time.Second
	// A running job whose log stopped growing for this long is stuck (the
	// helper is shell: it does not disappear silently).
	updateStaleAfter = 5 * time.Minute
	// A job record this old no longer blocks a new request (a dead timer
	// must not lock the console out forever).
	updateRefuseAfter = 30 * time.Minute
	updateLogKeep     = 40
	updateLogBytes    = 6000
)

func (s *Server) updateJobDir() string { return filepath.Join(s.cfg.DataDir, "update-jobs") }
func (s *Server) jobRecordPath(v string) string {
	return filepath.Join(s.updateJobDir(), "jobs", v+".json")
}
func (s *Server) jobLogPath(v string) string { return filepath.Join(s.updateJobDir(), v+".log") }
func (s *Server) jobReqPath(v string) string {
	return filepath.Join(s.updateJobDir(), "pending", v+".req")
}

func writeJobRecord(path string, rec jobRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJobRecord(path string) (jobRecord, bool) {
	var rec jobRecord
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &rec) != nil || !verRe.MatchString(rec.Version) {
		return rec, false
	}
	return rec, true
}

// latestJobRecord returns the most recently requested job, if any.
func (s *Server) latestJobRecord() (jobRecord, string, bool) {
	matches, _ := filepath.Glob(filepath.Join(s.updateJobDir(), "jobs", "*.json"))
	var best jobRecord
	var bestPath string
	found := false
	for _, p := range matches {
		rec, ok := readJobRecord(p)
		if !ok {
			continue
		}
		if !found || rec.StartedAt > best.StartedAt {
			best, bestPath, found = rec, p, true
		}
	}
	return best, bestPath, found
}

// latestJobSource returns the newest job, preferring the on-disk record written
// from this version on. Falling back to the helper log matters for exactly one
// case, and it is the case that ships this feature: a server updating *from* a
// release that kept job state in memory has no record at all, so the log is the
// only trace of the update that produced the binary now running.
func (s *Server) latestJobSource() (jobRecord, string, bool) {
	if rec, _, ok := s.latestJobRecord(); ok {
		return rec, "", true
	}
	matches, _ := filepath.Glob(filepath.Join(s.updateJobDir(), "*.log"))
	var best string
	var bestTime time.Time
	for _, m := range matches {
		if !verRe.MatchString(strings.TrimSuffix(filepath.Base(m), ".log")) {
			continue
		}
		fi, err := os.Stat(m)
		if err != nil {
			continue
		}
		if best == "" || fi.ModTime().After(bestTime) {
			best, bestTime = m, fi.ModTime()
		}
	}
	// Only a recent log can describe a job somebody is still watching.
	if best == "" || time.Since(bestTime) > 12*time.Hour {
		return jobRecord{}, "", false
	}
	ver := strings.TrimSuffix(filepath.Base(best), ".log")
	return jobRecord{ID: "upd-log-" + ver, Version: ver}, "", true
}

// helperLog is what the job log tells us. It understands both helper
// generations: the current one prints "STEP <phase> <detail>" lines, installs
// from before that only print human-readable progress ("checksum ok",
// "installed, restarting"), which map onto the same phases.
type helperLog struct {
	Lines    []string
	Result   string // ok|rolled_back|failed, "" while undecided
	Detail   string
	Mark     map[string]string // phase -> done|failed|active
	PhaseDet map[string]string // phase -> last detail seen
	ModTime  time.Time
}

func (hl *helperLog) mark(phase, state, detail string) {
	if hl.Mark == nil {
		hl.Mark = map[string]string{}
	}
	if hl.PhaseDet == nil {
		hl.PhaseDet = map[string]string{}
	}
	// "failed" sticks; "done" may upgrade an earlier "active" (the legacy
	// helper logs "installed, restarting" before "health ok" completes the
	// same phase).
	if cur := hl.Mark[phase]; cur != "failed" && !(state == "active" && cur != "") {
		hl.Mark[phase] = state
	}
	if detail != "" {
		hl.PhaseDet[phase] = detail
	}
}

// markState classifies a STEP detail string ("ok", "failed", "3 attempts…").
func markState(detail string) string {
	low := strings.ToLower(detail)
	switch {
	case strings.Contains(low, "fail"), strings.Contains(low, "mismatch"), strings.Contains(low, "error"):
		return "failed"
	case strings.HasPrefix(low, "ok"), strings.HasPrefix(low, "done"):
		return "done"
	}
	return "active"
}

func parseHelperLog(path string) (helperLog, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return helperLog{}, false
	}
	hl := helperLog{Mark: map[string]string{}, PhaseDet: map[string]string{}}
	if info, err := os.Stat(path); err == nil {
		hl.ModTime = info.ModTime()
	}
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(hl.Lines) < 5000 {
			hl.Lines = append(hl.Lines, line)
		}
		if i := strings.Index(line, updateResultMark); i >= 0 {
			parts := strings.SplitN(strings.TrimSpace(line[i+len(updateResultMark):]), " ", 2)
			hl.Result = parts[0]
			hl.Detail = ""
			if len(parts) > 1 {
				hl.Detail = parts[1]
			}
			continue
		}
		if i := strings.Index(line, updateStepMark); i >= 0 {
			parts := strings.SplitN(strings.TrimSpace(line[i+len(updateStepMark):]), " ", 2)
			name, detail := parts[0], ""
			if len(parts) > 1 {
				detail = parts[1]
			}
			switch name {
			case "queue":
				hl.mark("queue", "done", detail)
			case "download":
				hl.mark("download", markState(detail), detail)
			case "checksum":
				if markState(detail) == "failed" {
					hl.mark("download", "failed", detail)
				} else {
					hl.mark("download", "done", detail)
				}
			case "backup", "install":
				hl.mark("install", markState(detail), detail)
			case "install-done":
				hl.mark("install", "done", detail)
			case "restart":
				hl.mark("install", "done", "")
				hl.mark("restart", markState(detail), detail)
			case "health":
				hl.mark("restart", "done", "")
				hl.mark("health", markState(detail), detail)
			case "result":
				hl.mark("health", "done", "")
			}
			continue
		}
		// Legacy helper lines, mapped onto the same phases.
		switch {
		case strings.Contains(line, "target v") && strings.Contains(line, " arch "):
			hl.mark("queue", "done", strings.TrimPrefix(line, "[update] "))
		case strings.Contains(line, "checksum ok"):
			hl.mark("download", "done", "checksum ok")
		case strings.Contains(line, "download failed"),
			strings.Contains(line, "sums download failed"),
			strings.Contains(line, "checksum mismatch"):
			hl.mark("download", "failed", strings.TrimSpace(line))
		case strings.Contains(line, "installed, restarting"):
			hl.mark("install", "done", "installed")
			hl.mark("restart", "active", "systemctl restart agent-relay")
		case strings.Contains(line, "backup failed"), strings.Contains(line, "install failed"):
			hl.mark("install", "failed", strings.TrimSpace(line))
		case strings.Contains(line, "restart failed"):
			hl.mark("restart", "failed", strings.TrimSpace(line))
		case strings.Contains(line, "health ok"):
			hl.mark("restart", "done", "")
			hl.mark("health", "done", "health ok")
		case strings.Contains(line, "health failed"):
			hl.mark("health", "failed", strings.TrimSpace(line))
		}
	}
	return hl, true
}

func buildPhases(hl helperLog, status string) []updatePhase {
	defs := []struct{ name, label string }{
		{"queue", "排队等待 root timer 取件"},
		{"download", "下载并校验 release 文件"},
		{"install", "备份并替换二进制"},
		{"restart", "重启 agent-relay 服务"},
		{"health", "健康检查（失败自动回滚）"},
	}
	terminal := status == "ok" || status == "rolled_back" || status == "failed"
	// The phase that is still working (if any) is the one after the last
	// *completed* phase. Counting phases that merely started would light up
	// two rows at once ("downloading" and "installing" together).
	lastDone := -1
	for i, d := range defs {
		if hl.Mark[d.name] == "done" {
			lastDone = i
		}
	}
	out := make([]updatePhase, 0, len(defs))
	for i, d := range defs {
		st := hl.Mark[d.name]
		switch {
		case status == "ok":
			st = "done"
		case st == "failed" || st == "done" || st == "active":
			// as marked
		case terminal:
			st = "pending"
		case i == lastDone+1:
			st = "active"
		default:
			st = "pending"
		}
		out = append(out, updatePhase{Name: d.label, Status: st, Detail: hl.PhaseDet[d.name]})
	}
	return out
}

func logTail(lines []string) string {
	if len(lines) > updateLogKeep {
		lines = lines[len(lines)-updateLogKeep:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > updateLogBytes {
		out = "...[truncated]...\n" + out[len(out)-updateLogBytes:]
	}
	return out
}

func humanSince(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d 分 %d 秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d 小时 %d 分", int(d.Hours()), int(d.Minutes())%60)
}

// updateJobView is the pure core of the status endpoint, so the case that
// matters most — the helper restarted us mid-job and our memory is empty —
// is testable without a VPS.
func updateJobView(rec jobRecord, hl helperLog, haveLog, queued bool, running string, now time.Time) *updateJob {
	v := &updateJob{ID: rec.ID, Version: rec.Version, Status: "queued", StartedAt: rec.StartedAt}
	real := running != "" && running != "dev"
	switch {
	case hl.Result == "ok" || hl.Result == "rolled_back" || hl.Result == "failed":
		v.Status, v.Detail = hl.Result, hl.Detail
		if !hl.ModTime.IsZero() {
			v.EndedAt = hl.ModTime.Unix()
		}
	case real && running == rec.Version:
		// The binary was swapped and THIS process is the target version,
		// but the helper's result line never landed (killed mid-run). The
		// update did land — never leave the console saying "running".
		v.Status = "ok"
		v.Detail = "运行版本已是 " + rec.Version + "（helper 未写结果行）"
		v.EndedAt = now.Unix()
		if haveLog && !hl.ModTime.IsZero() {
			v.EndedAt = hl.ModTime.Unix()
		}
	case queued:
		v.Status = "queued"
	case haveLog:
		v.Status = "running"
	default:
		v.Status = "queued"
	}
	v.Confirmed = v.Status == "ok" && real && running == rec.Version
	end := now
	if v.EndedAt > 0 {
		end = time.Unix(v.EndedAt, 0)
	}
	if rec.StartedAt > 0 && end.Unix() > rec.StartedAt {
		v.Elapsed = end.Unix() - rec.StartedAt
	}
	v.Log = logTail(hl.Lines)
	v.Phases = buildPhases(hl, v.Status)

	switch {
	case v.Status == "queued" && rec.StartedAt > 0 && now.Sub(time.Unix(rec.StartedAt, 0)) > updateQueueGrace:
		v.Hint = "已排队 " + humanSince(now.Sub(time.Unix(rec.StartedAt, 0))) +
			" 仍未被取件：root timer（agent-relay-update.timer）可能没有在运行。" +
			"在服务器上执行 systemctl status agent-relay-update.timer，或重跑 deploy/install.sh 重新安装它。"
	case v.Status == "running" && haveLog && !hl.ModTime.IsZero() && now.Sub(hl.ModTime) > updateStaleAfter:
		v.Hint = "日志已 " + humanSince(now.Sub(hl.ModTime)) +
			" 没有更新，helper 可能卡住：在服务器上执行 journalctl -u agent-relay-update 查看。"
	case v.Status == "ok" && !v.Confirmed && real:
		v.Hint = "helper 报告成功，但当前进程仍在运行 " + running +
			"：服务可能没有重启（systemctl status agent-relay）。"
	}
	return v
}

// currentUpdateJob returns the console's view of the newest job (nil when no
// job was ever requested).
func (s *Server) currentUpdateJob() *updateJob {
	rec, _, ok := s.latestJobSource()
	if !ok {
		return nil
	}
	hl, haveLog := parseHelperLog(s.jobLogPath(rec.Version))
	_, qerr := os.Stat(s.jobReqPath(rec.Version))
	return updateJobView(rec, hl, haveLog, qerr == nil, web.BinaryVersion(), time.Now())
}

// updateInFlight explains why a new job must be refused ("" when idle). Disk
// state only: the process that accepted the job does not survive the restart
// the update performs, but the job keeps running.
func (s *Server) updateInFlight() string {
	matches, _ := filepath.Glob(filepath.Join(s.updateJobDir(), "pending", "*.req"))
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && time.Since(fi.ModTime()) < updateRefuseAfter {
			return "queued trigger " + filepath.Base(m) + " is still waiting for the root timer"
		}
	}
	rec, _, ok := s.latestJobSource()
	if !ok || rec.Audited {
		return ""
	}
	hl, haveLog := parseHelperLog(s.jobLogPath(rec.Version))
	if haveLog && hl.Result != "" {
		return ""
	}
	started := time.Unix(rec.StartedAt, 0)
	if rec.StartedAt == 0 && !hl.ModTime.IsZero() {
		started = hl.ModTime
	}
	if !started.IsZero() && time.Since(started) < updateRefuseAfter {
		return "job " + rec.ID + " for " + rec.Version + " is still in flight"
	}
	return ""
}

// startUpdateJob records the job on disk and drops the timer trigger. The
// record comes first: everything after this point may lose the process (the
// helper restarts it), and the console has to keep working. A non-empty busy
// reason means another job is already in flight.
func (s *Server) startUpdateJob(version string) (jobRecord, string, error) {
	if busy := s.updateInFlight(); busy != "" {
		return jobRecord{}, busy, nil
	}
	rec := jobRecord{
		ID:        fmt.Sprintf("upd-%d", time.Now().UnixNano()),
		Version:   version,
		StartedAt: time.Now().Unix(),
	}
	if err := writeJobRecord(s.jobRecordPath(version), rec); err != nil {
		return jobRecord{}, "", fmt.Errorf("write job record: %w", err)
	}
	reqPath := s.jobReqPath(version)
	if err := os.MkdirAll(filepath.Dir(reqPath), 0o755); err != nil {
		return jobRecord{}, "", fmt.Errorf("prepare trigger: %w", err)
	}
	if err := os.WriteFile(reqPath, []byte(rec.ID+"\n"), 0o644); err != nil {
		return jobRecord{}, "", fmt.Errorf("write trigger: %w", err)
	}
	return rec, "", nil
}

// watchUpdateJob follows the on-disk log until the helper writes its result,
// then books the outcome. Best effort: if this process is restarted before the
// result lands (the normal case), ReconcileUpdateJobs finishes the job at the
// next boot.
func (s *Server) watchUpdateJob(rec jobRecord, recPath string) {
	deadline := time.Now().Add(8 * time.Minute)
	for time.Now().Before(deadline) {
		if hl, ok := parseHelperLog(s.jobLogPath(rec.Version)); ok && hl.Result != "" {
			s.recordJobOutcome(recPath, rec, hl.Result, hl.Detail)
			return
		}
		time.Sleep(3 * time.Second)
	}
	log.Printf("update job %s: no result after 8m; leaving it to the next boot reconcile", rec.ID)
}

// recordJobOutcome writes the audit entry exactly once per job, whoever
// notices first: the watcher that triggered it, or the next boot.
func (s *Server) recordJobOutcome(recPath string, rec jobRecord, status, detail string) {
	if cur, ok := readJobRecord(recPath); ok && cur.Audited {
		return
	}
	rec.Audited, rec.Status, rec.Detail = true, status, detail
	if err := writeJobRecord(recPath, rec); err != nil {
		log.Printf("update job %s: record outcome: %v", rec.ID, err)
		return
	}
	action := "update.ok"
	if status != "ok" {
		action = "update." + status
	}
	_ = s.st.AppendAudit("admin", action,
		fmt.Sprintf("version=%s job=%s detail=%s", rec.Version, rec.ID, detail), time.Now().Unix())
	log.Printf("update job %s %s: %s", rec.ID, status, detail)
}

// ReconcileUpdateJobs finishes the bookkeeping for a job whose requesting
// process died in the restart that the update itself performs. Called once at
// boot: without it a successful web update would leave no audit entry and no
// way for the console to see the job it started.
func (s *Server) ReconcileUpdateJobs() {
	rec, recPath, ok := s.latestJobRecord()
	if !ok {
		// No record: an update from a release that kept jobs in memory (or a
		// log left by one). Book it once, then remember with a stub record so
		// later boots skip it.
		fromLog, _, have := s.latestJobSource()
		if !have {
			return
		}
		hl, ok := parseHelperLog(s.jobLogPath(fromLog.Version))
		if !ok || hl.Result == "" {
			return
		}
		fromLog.ID, fromLog.Audited = "upd-log-"+fromLog.Version, true
		if err := writeJobRecord(s.jobRecordPath(fromLog.Version), fromLog); err != nil {
			log.Printf("update job %s: record fallback outcome: %v", fromLog.ID, err)
			return
		}
		action := "update.ok"
		if hl.Result != "ok" {
			action = "update." + hl.Result
		}
		_ = s.st.AppendAudit("admin", action,
			fmt.Sprintf("version=%s job=%s detail=%s", fromLog.Version, fromLog.ID, hl.Detail), time.Now().Unix())
		log.Printf("update job %s %s (recovered from %s.log)", fromLog.ID, hl.Result, fromLog.Version)
		return
	}
	if rec.Audited {
		return
	}
	hl, haveLog := parseHelperLog(s.jobLogPath(rec.Version))
	if haveLog && hl.Result != "" {
		s.recordJobOutcome(recPath, rec, hl.Result, hl.Detail)
		return
	}
	if running := web.BinaryVersion(); running != "" && running != "dev" && running == rec.Version {
		s.recordJobOutcome(recPath, rec, "ok", "resumed after restart: running version is "+rec.Version)
		return
	}
	log.Printf("update job %s for %s: no result yet, keeping it visible in the console", rec.ID, rec.Version)
}

// ---- GET /admin/update/status ----
func (s *Server) handleAdminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"current": map[string]any{
			"version": web.BinaryVersion(), "tag": web.AssetVersion(),
			"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
			"prompt_version": prompts.PromptVersion,
		},
		"mode": updateMode(),
		"job":  s.currentUpdateJob(),
	})
}

// ---- POST /admin/update/check ----
// Empty version resolves to the latest GitHub release: the console's
// 检查 button works with no input. Explicit versions must match ^v[0-9].
func (s *Server) handleAdminUpdateCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version string `json:"version"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Version == "" {
		latest, err := fetchLatestRelease()
		if err != nil {
			writeErr(w, 502, "latest release unreachable: "+err.Error())
			return
		}
		req.Version = latest
	}
	if !verRe.MatchString(req.Version) {
		writeErr(w, 400, "version must look like v1.2.3")
		return
	}
	// Compare against running version for downgrade guard.
	// A dev build (no ldflags tag) skips the guard: anything goes in tests.
	cur := web.BinaryVersion()
	if cur != "" && cur != "dev" && !updateNewer(cur, req.Version) {
		writeErr(w, 400, "target must be newer than running "+cur+" (no downgrades)")
		return
	}
	// Best-effort protocol-change detection: fetch the release's staged
	// binary metadata via the GitHub API (no download yet). Offline or
	// unparseable → unknown=true, UI forces explicit acknowledgement.
	protoChange := false
	minChange := false
	unknown := false
	if info, err := fetchReleaseInfo(req.Version); err != nil {
		unknown = true
	} else {
		if info.Protocol != 0 && info.Protocol != s.cfg.Protocol {
			protoChange = true
		}
		if info.MinClient != 0 && info.MinClient != s.cfg.MinClient {
			minChange = true
		}
	}
	_ = s.st.AppendAudit("admin", "update.check",
		fmt.Sprintf("version=%s proto_change=%v min_change=%v unknown=%v", req.Version, protoChange, minChange, unknown),
		time.Now().Unix())
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": req.Version,
		"protocol_change": protoChange, "min_client_change": minChange,
		"unknown": unknown,
	})
}

// ---- POST /admin/update/apply ----
func (s *Server) handleAdminUpdateApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Version                   string `json:"version"`
		AcknowledgeProtocolChange bool   `json:"acknowledge_protocol_change"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if !verRe.MatchString(req.Version) {
		writeErr(w, 400, "version must look like v1.2.3")
		return
	}
	mode := updateMode()
	if mode != "systemd" {
		writeErr(w, 400, "web update applies to systemd installs only; docker mode: run the commands shown in the console")
		return
	}
	cur := web.BinaryVersion()
	if cur != "" && cur != "dev" && !updateNewer(cur, req.Version) {
		writeErr(w, 400, "target must be newer than running "+cur+" (no downgrades)")
		return
	}
	// Re-check protocol change server-side: the UI ack must be real.
	if info, err := fetchReleaseInfo(req.Version); err == nil {
		changed := (info.Protocol != 0 && info.Protocol != s.cfg.Protocol) ||
			(info.MinClient != 0 && info.MinClient != s.cfg.MinClient)
		if changed && !req.AcknowledgeProtocolChange {
			writeErr(w, 400, "target changes protocol/min_client: acknowledge_protocol_change required")
			return
		}
	} else if !req.AcknowledgeProtocolChange {
		writeErr(w, 400, "release metadata unreachable: acknowledge_protocol_change required to proceed blind")
		return
	}
	rec, busy, err := s.startUpdateJob(req.Version)
	if busy != "" {
		writeJSON(w, 409, map[string]any{"ok": false, "error": "update_in_progress", "detail": busy})
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = s.st.AppendAudit("admin", "update.apply",
		fmt.Sprintf("version=%s job=%s", req.Version, rec.ID), time.Now().Unix())
	go s.watchUpdateJob(rec, s.jobRecordPath(req.Version))
	writeJSON(w, 200, map[string]any{"ok": true, "job_id": rec.ID})
}

// updateNewer reports whether target is newer than current. Versions are
// vMAJOR.MINOR.PATCH with optional suffix; compare numerically, suffix
// ignored (a suffix never outranks its base).
func updateNewer(cur, target string) bool {
	return compareVersions(target, cur) > 0
}

func compareVersions(a, b string) int {
	pa := parseVersion(a)
	pb := parseVersion(b)
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			if pa[i] > pb[i] {
				return 1
			}
			return -1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	var out [3]int
	v = strings.TrimPrefix(v, "v")
	v = strings.SplitN(v, "-", 2)[0]
	v = strings.SplitN(v, "+", 2)[0]
	parts := strings.Split(v, ".")
	for i := 0; i < 3 && i < len(parts); i++ {
		var n int
		fmt.Sscanf(parts[i], "%d", &n)
		out[i] = n
	}
	return out
}

// releaseInfo is the subset of GitHub release metadata we need.
type releaseInfo struct {
	Protocol  int
	MinClient int
}

// fetchLatestRelease resolves the newest v* release tag from the GitHub
// API (redirects to the actual latest, drafts/prereleases excluded by the
// /latest endpoint). Used when the console checks with no version input.
func fetchLatestRelease() (string, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get("https://api.github.com/repos/AlixWang/agent-relay/releases/latest")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github api %d", resp.StatusCode)
	}
	var body struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if !verRe.MatchString(body.Tag) {
		return "", fmt.Errorf("bad tag %q", body.Tag)
	}
	return body.Tag, nil
}

// fetchReleaseInfo asks the GitHub API for a tag's release notes and scans
// for `protocol: N` / `min_client: N` markers. Releases predate the marker
// convention → zeros (treated as unknown by the caller when both are 0? no:
// zeros mean "no info", caller compares non-zero only).
func fetchReleaseInfo(version string) (*releaseInfo, error) {
	// repoSlug is fixed at build: same repo that serves this binary.
	url := "https://api.github.com/repos/AlixWang/agent-relay/releases/tags/" + version
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github api %d", resp.StatusCode)
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	info := &releaseInfo{}
	for _, line := range strings.Split(body.Body, "\n") {
		line = strings.TrimSpace(line)
		var n int
		if strings.HasPrefix(line, "protocol:") {
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "protocol:")), "%d", &n)
			info.Protocol = n
		}
		if strings.HasPrefix(line, "min_client:") {
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(line, "min_client:")), "%d", &n)
			info.MinClient = n
		}
	}
	return info, nil
}

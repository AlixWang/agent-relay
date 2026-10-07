package gateway

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/web"
)

// Update mode detection: docker containers carry /.dockerenv; the binary
// path is baked by install.sh/systemd vs ENTRYPOINT.
func updateMode() string {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return "docker"
	}
	if exe, err := os.Executable(); err == nil && strings.HasPrefix(exe, "/usr/local/bin/") {
		return "systemd"
	}
	return "unknown"
}

var verRe = regexp.MustCompile(`^v[0-9][A-Za-z0-9._-]*$`)

type updateJob struct {
	ID        string
	Version   string
	Status    string // running|ok|rolled_back|failed
	Log       string
	StartedAt int64
	EndedAt   int64
}

type updateManager struct {
	mu   sync.Mutex
	jobs map[string]*updateJob
	cur  string // running job id, "" when idle
}

func newUpdateManager() *updateManager {
	return &updateManager{jobs: map[string]*updateJob{}}
}

func (m *updateManager) start(version string) (*updateJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cur != "" {
		return nil, false
	}
	j := &updateJob{
		ID:      fmt.Sprintf("upd-%d", time.Now().UnixNano()),
		Version: version, Status: "running", StartedAt: time.Now().Unix(),
	}
	m.jobs[j.ID] = j
	m.cur = j.ID
	return j, true
}

func (m *updateManager) finish(id, status, logTail string, endedAt int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		j.Status = status
		j.Log = logTail
		j.EndedAt = endedAt
	}
	if m.cur == id {
		m.cur = ""
	}
}

func (m *updateManager) get(id string) *updateJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

func (m *updateManager) latest() *updateJob {
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *updateJob
	for _, j := range m.jobs {
		if best == nil || j.StartedAt > best.StartedAt {
			best = j
		}
	}
	return best
}

// updateMgr lives on Server lazily (avoids changing New's signature).
func (s *Server) updateMgr() *updateManager {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updMgr == nil {
		s.updMgr = newUpdateManager()
	}
	return s.updMgr
}

// ---- GET /admin/update/status ----
func (s *Server) handleAdminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	mgr := s.updateMgr()
	job := mgr.latest()
	var jobView any
	if job != nil {
		mgr.mu.Lock()
		cp := *job
		mgr.mu.Unlock()
		jobView = map[string]any{
			"id": cp.ID, "version": cp.Version, "status": cp.Status,
			"log": cp.Log, "started_at": cp.StartedAt, "ended_at": cp.EndedAt,
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"current": map[string]any{
			"version": web.BinaryVersion(), "tag": web.AssetVersion(),
			"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
			"prompt_version": prompts.PromptVersion,
		},
		"mode": updateMode(),
		"job":  jobView,
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
	mgr := s.updateMgr()
	job, ok := mgr.start(req.Version)
	if !ok {
		writeJSON(w, 409, map[string]any{"ok": false, "error": "update_in_progress"})
		return
	}
	_ = s.st.AppendAudit("admin", "update.apply",
		fmt.Sprintf("version=%s job=%s", req.Version, job.ID), time.Now().Unix())
	go s.runUpdateJob(job)
	writeJSON(w, 200, map[string]any{"ok": true, "job_id": job.ID})
}

// runUpdateJob triggers the root oneshot service and tails the on-disk
// job log until the helper writes its UPDATE_RESULT line.
// Why a service, not `sudo helper apply <ver>`: the gateway inherits the
// main unit's ProtectSystem=strict mount namespace (/usr read-only) and
// sudo cannot escape it — backup/install fail with Read-only file system.
// The oneshot unit runs as root in a clean namespace; sudoers allows only
// the parameterless `systemctl start`, the version travels via
// `systemctl set-environment UPDATE_VERSION=<ver>` (also whitelisted).
func (s *Server) runUpdateJob(job *updateJob) {
	jobDir := filepath.Join(s.cfg.DataDir, "update-jobs")
	_ = os.MkdirAll(jobDir, 0o755)
	logPath := filepath.Join(jobDir, job.Version+".log")

	run := func(name string, args ...string) (string, error) {
		cmd := exec.Command(name, args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	status, detail := "failed", "trigger failed"
	// set-environment is separate so the start rule stays parameterless.
	if out, err := run("sudo", "-n", "systemctl", "set-environment", "UPDATE_VERSION="+job.Version); err != nil {
		detail = "set-environment: " + strings.TrimSpace(out)
	} else if out, err := run("sudo", "-n", "systemctl", "start", "agent-relay-update.service"); err != nil {
		detail = "start: " + strings.TrimSpace(out)
	} else {
		// The helper restarts THIS process mid-job; poll the on-disk log
		// for the terminal line instead of waiting on a child.
		status, detail = s.waitJobResult(logPath, job.Version)
	}
	tail := s.readJobTail(logPath)
	now := time.Now().Unix()
	s.updateMgr().finish(job.ID, status, tail, now)
	action := "update.ok"
	if status != "ok" {
		action = "update." + status // update.rolled_back | update.failed
	}
	_ = s.st.AppendAudit("admin", action,
		fmt.Sprintf("version=%s job=%s detail=%s", job.Version, job.ID, detail), now)
	log.Printf("update job %s %s: %s", job.ID, status, detail)
}

// waitJobResult polls the on-disk job log for the helper's UPDATE_RESULT
// line. Returns when found or after ~5 minutes (helper has its own curl
// timeouts; a missing line means the service never ran).
func (s *Server) waitJobResult(logPath, version string) (string, string) {
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(logPath); err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "UPDATE_RESULT ") {
					parts := strings.SplitN(line, " ", 3)
					if len(parts) == 3 {
						return parts[1], parts[2]
					}
				}
			}
		}
		time.Sleep(3 * time.Second)
	}
	return "failed", "timeout waiting for " + version + " result"
}

// readJobTail returns the last 8KB of the on-disk job log for the console.
func (s *Server) readJobTail(logPath string) string {
	data, err := os.ReadFile(logPath)
	if err != nil || len(data) == 0 {
		return "(no job log yet)"
	}
	tail := string(data)
	if len(tail) > 8000 {
		tail = "...[truncated]...\n" + tail[len(tail)-8000:]
	}
	return tail
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

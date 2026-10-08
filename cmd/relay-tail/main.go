// Command relay-tail is the dumb-pipe receiver prototype (companion to the
// tail/poll shell scripts, not a replacement for wake policy).
//
// It owns exactly the "VPS <-> local" segment: one transport (--transport
// poll|sse), shared message/prompt handling, atomic cursor persistence,
// backoff reconnect. It emits normalized events on stdout (one JSON object
// per line, same {tasks:[...]} shape as relay-poll.sh) and never invokes
// wake policy itself — thin shell (hook watchdog) reads the event stream
// and decides prompt_update dedup, presence, and runtime wake calls.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	transport = flag.String("transport", "poll", "receiver transport: poll or sse")
	pollSecs  = flag.Int("interval", 5, "poll interval seconds (poll mode)")
)

// clientVersion is the receiver build tag (§8.9), baked at release time:
// -ldflags "-X main.clientVersion=v0.7.0". Reported in every heartbeat for
// display/download. Dev builds report "" (= not reporting, never nudged).
var clientVersion = ""

// receiverRev is the receiver *source* revision (§8.9), baked from the same
// source hash the server uses: -ldflags "-X main.receiverRev=<hash>". The
// server nudges via client_update only when this differs from its own value,
// so a release that does not touch this receiver (docs/prompt/server-only)
// never asks anyone to swap binaries. Dev builds report "" (= unknown).
var receiverRev = ""

type cfg struct {
	relay      string
	base       string
	ident      string
	token      string
	http       *http.Client
	proxyArgs  string // optional http proxy (sandbox tunnel quirk)
	seqPath    string
	verPath    string
	stagedPath string
	updatePath string
	changePath string
	// clientVerPath stages a client_update wake (analogue of stagedPath
	// for prompts): the reported version confirmed by the worker after
	// upgrading the binary. Dedups the nudge to once per Release.
	clientVerPath string
}

func main() {
	flag.Parse()
	c, err := loadCfg()
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay-tail: %v\n", err)
		os.Exit(1)
	}
	switch *transport {
	case "poll":
		os.Exit(c.runPoll())
	case "sse":
		os.Exit(c.runSSE())
	default:
		fmt.Fprintf(os.Stderr, "relay-tail: bad --transport %q (poll|sse)\n", *transport)
		os.Exit(1)
	}
}

func loadCfg() (*cfg, error) {
	relay := os.Getenv("RELAY")
	if relay == "" {
		relay = "http://100.71.61.96:18789"
	}
	base := os.Getenv("BASE")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, "workspace", "task-relay")
	}
	readTrim := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	ident := readTrim(filepath.Join(base, "identity"))
	if !validIdent(ident) {
		return nil, fmt.Errorf("bad or missing identity in %s/identity", base)
	}
	token := readTrim(filepath.Join(base, ".token"))
	if token == "" {
		return nil, fmt.Errorf("no token at %s/.token", base)
	}
	tr := &http.Transport{}
	tr.Proxy = proxyForRelay(relay, os.Getenv("HTTPS_PROXY"))
	return &cfg{
		relay: relay, base: base, ident: ident, token: token,
		http:          &http.Client{Transport: tr, Timeout: 30 * time.Second},
		seqPath:       filepath.Join(base, ".last_seq"),
		verPath:       filepath.Join(base, ".prompt_version"),
		stagedPath:    filepath.Join(base, ".prompt_version.staged"),
		updatePath:    filepath.Join(base, "prompt-update.md"),
		changePath:    filepath.Join(base, "prompt-changes.json"),
		clientVerPath: filepath.Join(base, ".client_version"),
	}, nil
}

// proxyForRelay decides the HTTP proxy for relay traffic.
//
// Sandbox quirk: some Muse sandboxes only reach tailnet addresses through a
// tunnel proxy derived from HTTPS_PROXY as ${HTTPS_PROXY%:*}:3130. That
// rewrite must ONLY apply when the relay itself is a tailnet/private
// address — applying it unconditionally breaks public relays behind a normal
// egress proxy (the :3130 port is dead there, all requests fail).
//
//   - HTTPS_PROXY unset/empty -> direct, no proxy
//   - relay is tailnet/private -> ${HTTPS_PROXY%:*}:3130 tunnel
//   - otherwise -> HTTPS_PROXY as-is (standard egress proxy)
func proxyForRelay(relay, px string) func(*http.Request) (*url.URL, error) {
	if px == "" {
		return nil
	}
	target := px
	if isPrivateRelay(relay) {
		if i := strings.LastIndex(px, ":"); i > 0 {
			target = px[:i] + ":3130"
		}
	}
	u, err := url.Parse(target)
	if err != nil {
		return nil
	}
	return http.ProxyURL(u)
}

// isPrivateRelay reports whether the relay URL points at a tailnet or other
// private address: Tailscale CGNAT 100.64/10, RFC1918, loopback, .local etc.
func isPrivateRelay(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") ||
		strings.HasSuffix(host, ".internal") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // public DNS name
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return true
	}
	if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] < 128 {
		return true // Tailscale 100.64.0.0/10 (not covered by IsPrivate)
	}
	return false
}

func validIdent(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, r := range s {
		ok := r == '-' || r == '_' ||
			(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
		if !ok || (i == 0 && (r == '-' || r == '_')) {
			return false
		}
	}
	return true
}

// ---- shared core: cursor, heartbeat/prompt, wake emit ----

func (c *cfg) readSeq() int64 {
	b, err := os.ReadFile(c.seqPath)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if n < 0 {
		return 0
	}
	return n
}

// writeSeq persists the cursor atomically (tmp + rename): a torn .last_seq
// replays or skips messages after a crash — the jq-overwrite in shell had
// this hole.
func (c *cfg) writeSeq(n int64) {
	tmp := c.seqPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(n, 10)), 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, c.seqPath)
}

func (c *cfg) readVer() int {
	b, err := os.ReadFile(c.verPath)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if n < 0 {
		return 0
	}
	return n
}

// readStaged returns the staged (unconfirmed) prompt version, or 0.
func (c *cfg) readStaged() int {
	b, err := os.ReadFile(c.stagedPath)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if n < 0 {
		return 0
	}
	return n
}

func (c *cfg) auth(r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+c.token)
}

// heartbeatOnce mirrors relay-poll.sh §1b: report prompt_version, stage
// prompt-update.md + prompt-changes.json + .prompt_version.staged, return
// the prompt_update wake event ("" when none). Also reports clientVersion
// (§8.9) and returns a client_update wake event when the server has a
// newer Release ("" when none). Best-effort: "" on any error.
// Dedup: an already-staged version wakes only once — without this a peer
// that hasn't confirmed yet gets re-woken every round (visible as
// per-round spam in long-lived receivers). Same rule for client_update:
// once .client_version matches the nudged release, stay silent; the worker
// confirms by upgrading the binary (which reports the new version).
func (c *cfg) heartbeatOnce() (promptEvent, clientEvent string) {
	ver := c.readVer()
	body, _ := json.Marshal(map[string]any{
		"id": c.ident, "prompt_version": ver, "client_version": clientVersion,
		"client_rev": receiverRev,
	})
	req, _ := http.NewRequest("POST", c.relay+"/heartbeat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	var hb struct {
		PromptUpdate  bool   `json:"prompt_update"`
		PromptVersion int    `json:"prompt_version"`
		ClientUpdate  bool   `json:"client_update"`
		ClientVersion string `json:"client_version"`
		ReceiverRev   string `json:"receiver_rev"`
	}
	if json.NewDecoder(resp.Body).Decode(&hb) != nil {
		return "", ""
	}
	if !hb.PromptUpdate || hb.PromptVersion <= ver {
		promptEvent = ""
	} else if staged := c.readStaged(); staged == hb.PromptVersion {
		promptEvent = "" // already staged, worker hasn't confirmed yet
	} else {
		req2, _ := http.NewRequest("GET", c.relay+"/prompts/current", nil)
		c.auth(req2)
		resp2, err := c.http.Do(req2)
		if err != nil {
			return "", ""
		}
		defer resp2.Body.Close()
		var pu struct {
			Prompt  string           `json:"prompt"`
			Changes []map[string]any `json:"changes"`
		}
		if json.NewDecoder(resp2.Body).Decode(&pu) != nil || pu.Prompt == "" {
			return "", ""
		}
		_ = os.WriteFile(c.updatePath, []byte(pu.Prompt), 0o644)
		ch, _ := json.Marshal(pu.Changes)
		if ch == nil {
			ch = []byte("[]")
		}
		_ = os.WriteFile(c.changePath, ch, 0o644)
		_ = os.WriteFile(c.stagedPath, []byte(strconv.Itoa(hb.PromptVersion)), 0o644)
		var sums []string
		for _, e := range pu.Changes {
			if s, ok := e["summary"].(string); ok {
				sums = append(sums, s)
			}
		}
		ev, _ := json.Marshal(map[string]any{
			"prompt_update": true, "version": hb.PromptVersion,
			"changes": strings.Join(sums, " | "),
		})
		promptEvent = string(ev)
	}
	// Receiver client nudge (§8.9): wake once per needed update. The
	// server compares receiver revisions; we re-check locally as a guard
	// (an old server, or a stale nudge, must not make us swap binaries
	// for an unchanged receiver). Dedup via .client_version: the worker
	// confirms by upgrading the binary (which then reports the new
	// revision and the nudge stops).
	needUpdate := hb.ClientUpdate && hb.ClientVersion != "" && hb.ClientVersion != clientVersion
	if needUpdate && receiverRev != "" && hb.ReceiverRev != "" && hb.ReceiverRev == receiverRev {
		needUpdate = false // same receiver source: nothing to upgrade
	}
	if needUpdate {
		if confirmed := c.readConfirmedClient(); confirmed != hb.ClientVersion {
			_ = os.WriteFile(c.clientVerPath, []byte(hb.ClientVersion), 0o644)
			payload := map[string]any{
				"client_update": true, "version": hb.ClientVersion,
				"download": "/clients/relay-tail?arch=<amd64|arm64>",
			}
			if hb.ReceiverRev != "" {
				payload["rev"] = hb.ReceiverRev
			}
			ce, _ := json.Marshal(payload)
			clientEvent = string(ce)
		}
	}
	return promptEvent, clientEvent
}

// readConfirmedClient returns the staged client_update version (unconfirmed
// nudge), or "" when none.
func (c *cfg) readConfirmedClient() string {
	b, err := os.ReadFile(c.clientVerPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// emitWake prints one normalized wake line: {tasks:[...]} plus optional
// prompt_update / client_update events. Same shape as the shell scripts so
// the thin wake shell needs no change. Returns false when there is nothing
// to wake on.
func emitWake(tasks []map[string]any, events ...string) bool {
	hasEvent := false
	for _, e := range events {
		if e != "" {
			hasEvent = true
			break
		}
	}
	if len(tasks) == 0 && !hasEvent {
		return false
	}
	out := map[string]any{"tasks": tasks}
	for _, e := range events {
		if e == "" {
			continue
		}
		var v map[string]any
		if json.Unmarshal([]byte(e), &v) != nil {
			continue
		}
		for k, val := range v {
			out[k] = val
		}
	}
	b, _ := json.Marshal(out)
	fmt.Println(string(b))
	return true
}

// ---- poll mode: one heartbeat + one pull per tick ----

func (c *cfg) runPoll() int {
	interval := *pollSecs
	if interval <= 0 {
		interval = 5
	}
	for {
		promptEvent, clientEvent := c.heartbeatOnce()
		since := c.readSeq()
		req, _ := http.NewRequest("GET",
			fmt.Sprintf("%s/messages?for=%s&since=%d", c.relay, c.ident, since), nil)
		c.auth(req)
		resp, err := c.http.Do(req)
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay-tail: relay unreachable, backing off\n")
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}
		var pr struct {
			Items     []map[string]any `json:"items"`
			NextSince int64            `json:"next_since"`
		}
		code := resp.StatusCode
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		switch code {
		case 200:
		case 401, 426:
			fmt.Fprintf(os.Stderr, "relay-tail: %d — token revoked or protocol outdated; ask your human to re-onboard\n", code)
			return 2
		default:
			fmt.Fprintf(os.Stderr, "relay-tail: relay unreachable (http %d), backing off\n", code)
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}
		c.writeSeq(pr.NextSince)
		emitWake(pr.Items, promptEvent, clientEvent)
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// ---- sse mode: held stream, incremental parse ----

func (c *cfg) runSSE() int {
	backoff := time.Second
	go c.heartbeatLoop()
	for {
		since := c.readSeq()
		promptEvent, clientEvent := c.heartbeatOnce()
		if promptEvent != "" || clientEvent != "" {
			// Surface staged instructions / client nudges without killing
			// the stream: same wake line the shell heartbeat used to emit.
			emitWake(nil, promptEvent, clientEvent)
		}
		code := c.holdStream(since)
		switch code {
		case 401, 426:
			fmt.Fprintf(os.Stderr, "relay-tail: %d — token revoked or protocol outdated; ask your human to re-onboard\n", code)
			return 2
		case 429:
			time.Sleep(30 * time.Second)
			continue
		case 200:
			// Delivered frames reset backoff; silent drops back off.
			backoff = time.Second
		default:
			time.Sleep(backoff)
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
}

func (c *cfg) heartbeatLoop() {
	for {
		time.Sleep(60 * time.Second)
		if pe, ce := c.heartbeatOnce(); pe != "" || ce != "" {
			emitWake(nil, pe, ce)
		}
	}
}

// holdStream holds GET /messages/stream open and parses frames
// incrementally: every data frame emits a wake line immediately and every
// id line advances the persisted cursor. Returns the terminal HTTP status
// (200 clean/delivered, 401/426 auth, 429 capped, 0 network).
func (c *cfg) holdStream(since int64) int {
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/messages/stream?for=%s&since=%d", c.relay, c.ident, since), nil)
	c.auth(req)
	// No client timeout: the server pings every ~keepalive; idle TCP stays
	// alive through proxies. A separate client without Timeout.
	cl := *c.http
	cl.Timeout = 0
	resp, err := cl.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	delivered := false
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var datas []string
	flush := func(idLine string) {
		if len(datas) == 0 {
			return
		}
		var tasks []map[string]any
		for _, d := range datas {
			var one map[string]any
			if json.Unmarshal([]byte(d), &one) == nil {
				tasks = append(tasks, one)
			}
		}
		datas = datas[:0]
		if emitWake(tasks, "") {
			delivered = true
		}
		_ = idLine
	}
	var lastID string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			if id := strings.TrimSpace(strings.TrimPrefix(line, "id: ")); isDigits(id) {
				lastID = id
				if n, err := strconv.ParseInt(id, 10, 64); err == nil {
					c.writeSeq(n)
				}
			}
		case strings.HasPrefix(line, "data: "):
			datas = append(datas, strings.TrimPrefix(line, "data: "))
		case line == "":
			flush(lastID) // blank line = end of frame: emit NOW
		}
	}
	if !delivered {
		return -1 // silent disconnect: caller backs off
	}
	return 200
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

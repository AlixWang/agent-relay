package muse

// These tests pin the observability contract for direct-chat baseline failures:
// the public error stays muse_history_unavailable, while audit/log carry the
// underlying chatFailure code (and HTTP status when present). No test body,
// session id, token or cookie may appear in either place.

import (
	"bytes"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

// lockedLogBuffer is safe for the worker goroutine and the test goroutine.
type lockedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureChatLog(t *testing.T) *lockedLogBuffer {
	t.Helper()
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	buf := &lockedLogBuffer{}
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})
	return buf
}

func TestChatBaselineFailureAuditsAndLogsUnderlyingReason(t *testing.T) {
	cases := []struct {
		name       string
		wantReason string
		wantStatus int
		timeout    time.Duration
		handler    func(*musetest.Emitter) musetest.Response
		secret     string
	}{
		{
			name:       "sessions_list_404",
			wantReason: "muse_protocol_drift",
			wantStatus: http.StatusNotFound,
			timeout:    time.Second,
			handler: func(*musetest.Emitter) musetest.Response {
				return musetest.Response{Status: http.StatusNotFound, Body: []byte("upstream-body-secret"), EndBody: true}
			},
			secret: "upstream-body-secret",
		},
		{
			name:       "sessions_list_502",
			wantReason: "muse_upstream_error",
			wantStatus: http.StatusBadGateway,
			timeout:    time.Second,
			handler: func(*musetest.Emitter) musetest.Response {
				return musetest.Response{Status: http.StatusBadGateway, Body: []byte(`{"error":"upstream-body-secret"}`), EndBody: true}
			},
			secret: "upstream-body-secret",
		},
		{
			name:       "sessions_list_json_drift",
			wantReason: "muse_protocol_drift",
			timeout:    time.Second,
			handler: func(*musetest.Emitter) musetest.Response {
				return musetest.Response{Status: 200, Body: []byte(`{"sessions":"body-secret"}`), EndBody: true}
			},
			secret: "body-secret",
		},
		{
			name:       "sessions_list_no_main_chat",
			wantReason: "muse_main_chat_unresolved",
			timeout:    time.Second,
			handler: func(*musetest.Emitter) musetest.Response {
				return musetest.Response{Status: 200, Body: []byte(
					`{"sessions":[{"session_id":"side-secret-id","is_primary":false,"is_thread":true}]}`), EndBody: true}
			},
			secret: "side-secret-id",
		},
		{
			name:       "sessions_list_noise_timeout",
			wantReason: "muse_upstream_timeout",
			timeout:    80 * time.Millisecond,
			handler: func(*musetest.Emitter) musetest.Response {
				return musetest.Response{Status: 200, Body: []byte(`{"sessions":[]}`), EndBody: true}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldTimeout := chatSessionsTimeout
			chatSessionsTimeout = tc.timeout
			t.Cleanup(func() { chatSessionsTimeout = oldTimeout })

			logs := captureChatLog(t)
			env := newChatTestEnv(t)
			ws := musetest.New(t)

			release := make(chan struct{})
			releaseOnce := sync.Once{}
			defer releaseOnce.Do(func() { close(release) })
			ws.SetStreamHandler(func(req musetest.Request, em *musetest.Emitter) musetest.Response {
				if req.Verb == http.MethodGet && req.Path == chatSessionsPath {
					if tc.name == "sessions_list_noise_timeout" {
						<-release
					}
					return tc.handler(em)
				}
				return musetest.Response{Status: http.StatusNotFound, EndBody: true}
			})

			sess := newLiveChatSessionForTest(t, ws.URL)
			view := env.start(t, &liveChatGateway{sess: sess}, ChatStartRequest{Text: "pwd"})
			got := waitChatStatus(t, env.svc, view.ChatID, ChatStatusFailed)
			waitChatDone(t, env.svc, view.ChatID)

			if got.Error == nil || got.Error.Code != "muse_history_unavailable" {
				t.Fatalf("public error changed: %+v", got.Error)
			}
			for _, req := range ws.Requests() {
				if req.Verb == http.MethodPost && req.Path == "/chat/stream" {
					t.Fatalf("baseline failure wrote a chat stream: %+v", req)
				}
			}

			rows, err := env.st.ListAudit("", "muse.chat.failed", 0, 20)
			if err != nil || len(rows) != 1 {
				t.Fatalf("failed audit rows=%d err=%v", len(rows), err)
			}
			detail := rows[0].Detail
			wants := []string{
				"stage=sending",
				"error_class=muse_history_unavailable",
				"reason=" + tc.wantReason,
			}
			if tc.wantStatus > 0 {
				wants = append(wants, "status="+strconv.Itoa(tc.wantStatus))
			}
			for _, want := range wants {
				if !strings.Contains(detail, want) {
					t.Fatalf("audit detail %q missing %q", detail, want)
				}
			}

			out := logs.String()
			for _, want := range []string{
				"session list failed",
				"chat=" + view.ChatID,
				"peer=alice",
				"account=" + env.account.ID,
				"stage=session_list",
				"reason=" + tc.wantReason,
				"baseline history failed",
				"stage=sending",
				"error_class=muse_history_unavailable",
				"failure_stage=session_list",
			} {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			if tc.wantStatus > 0 && !strings.Contains(out, "status="+strconv.Itoa(tc.wantStatus)) {
				t.Fatalf("log missing status=%d:\n%s", tc.wantStatus, out)
			}
			if tc.name == "sessions_list_no_main_chat" {
				for _, want := range []string{"sessions=1", "primary_field=1", "thread_field=1", "entries=pT", "resolved=no"} {
					if !strings.Contains(out, want) {
						t.Fatalf("shape summary missing %q:\n%s", want, out)
					}
				}
			}
			if tc.secret != "" && (strings.Contains(detail, tc.secret) || strings.Contains(out, tc.secret)) {
				t.Fatalf("secret/body leaked: audit=%q logs=%q", detail, out)
			}
		})
	}
}

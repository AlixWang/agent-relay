package muse

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

func dialChatTestConn(t *testing.T, wsURL string) *chatConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := dialNoiseConn(ctx, &http.Client{Timeout: 2 * time.Second}, wsURL, "vm-test", "token-test")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.handshake(ctx, 3*time.Second); err != nil {
		conn.close()
		t.Fatalf("handshake: %v", err)
	}
	cc := newChatConn(context.Background(), conn)
	t.Cleanup(cc.close)
	return cc
}

func collectSink(t *testing.T, sink *chatSink) (body []byte, frames int, sawEnd bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		select {
		case f := <-sink.ch:
			if f.Err != nil {
				t.Fatalf("stream error: %v", f.Err)
			}
			frames++
			if f.Response != nil {
				body = append(body, f.Response.Body...)
				if f.Response.EndBody {
					return body, frames, true
				}
			}
			if f.BodyChunk != nil {
				body = append(body, f.BodyChunk.Data...)
				if f.BodyChunk.EndBody {
					return body, frames, true
				}
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for stream end; frames=%d body=%q", frames, body)
		}
	}
}

func TestChatConnPostBodyAndInterleavedStreams(t *testing.T) {
	ws := musetest.New(t)
	var streamReq musetest.Request
	var historySeen atomicInt
	ws.SetStreamHandler(func(req musetest.Request, em *musetest.Emitter) musetest.Response {
		switch {
		case req.Path == "/chat/stream":
			streamReq = req
			go func() {
				time.Sleep(30 * time.Millisecond)
				_ = em.Emit(musetest.Response{Chunk: &musetest.Chunk{Data: []byte(`{"line":2}` + "\n"), EndBody: true}})
				// A late frame after end_body must be counted as orphan.
				time.Sleep(20 * time.Millisecond)
				_ = em.Emit(musetest.Response{Chunk: &musetest.Chunk{Data: []byte("late")}})
			}()
			return musetest.Response{Status: 200, Body: []byte(`{"line":1}` + "\n"), NoEnd: true}
		case strings.HasPrefix(req.Path, "/chat/history"):
			historySeen.Add(1)
			return musetest.Response{Status: 200, Body: []byte(`{"chat_events":[]}`), EndBody: true}
		default:
			return musetest.Response{Status: 404, EndBody: true}
		}
	})
	cc := dialChatTestConn(t, ws.URL)

	body, _ := json.Marshal(map[string]any{"items": []any{map[string]string{"type": "text", "text": "hi"}}})
	sink, _, err := cc.openStream(context.Background(), http.MethodPost, "/chat/stream", body, nil)
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	// A unary history call must be served while the stream is open.
	resp, err := cc.call(context.Background(), http.MethodGet, "/chat/history?limit=20", nil, nil)
	if err != nil {
		t.Fatalf("history during stream: %v", err)
	}
	if resp.Status != 200 || !strings.Contains(string(resp.Body), "chat_events") {
		t.Fatalf("history response: %d %q", resp.Status, resp.Body)
	}
	if historySeen.Load() != 1 {
		t.Fatalf("history requests: %d", historySeen.Load())
	}
	echo, frames, ended := collectSink(t, sink)
	cc.unregisterStream(sink)
	if !ended || frames < 2 {
		t.Fatalf("stream frames=%d ended=%v body=%q", frames, ended, echo)
	}
	if !strings.Contains(string(echo), `"line":2`) {
		t.Fatalf("body chunks not assembled: %q", echo)
	}
	if streamReq.Verb != http.MethodPost || streamReq.Path != "/chat/stream" {
		t.Fatalf("stream request: %+v", streamReq)
	}
	if streamReq.Headers["content-type"] != "application/json" || streamReq.Headers["x-request-id"] == "" {
		t.Fatalf("stream headers: %+v", streamReq.Headers)
	}
	var sent map[string]any
	if err := json.Unmarshal(streamReq.Body, &sent); err != nil {
		t.Fatalf("stream JSON body: %v", err)
	}
	if sent["items"] == nil {
		t.Fatalf("body not preserved: %s", streamReq.Body)
	}
	deadline := time.Now().Add(time.Second)
	for cc.orphanCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cc.orphanCount() < 1 {
		t.Fatal("late frame after end_body was not counted as orphan")
	}
}

func TestChatConnDispatchesConcurrentUnaryCalls(t *testing.T) {
	ws := musetest.New(t)
	ws.SetStreamHandler(func(req musetest.Request, em *musetest.Emitter) musetest.Response {
		delay := time.Duration(0)
		if req.Path == "/slow" {
			delay = 80 * time.Millisecond
		}
		go func() {
			time.Sleep(delay)
			_ = em.Emit(musetest.Response{Chunk: &musetest.Chunk{
				Data: []byte(`{"path":"` + req.Path + `"}`), EndBody: true}})
		}()
		return musetest.Response{Status: 200, NoEnd: true}
	})
	cc := dialChatTestConn(t, ws.URL)

	type result struct {
		path string
		resp *appResponse
		err  error
	}
	results := make(chan result, 2)
	for _, path := range []string{"/slow", "/fast"} {
		go func(path string) {
			resp, err := cc.call(context.Background(), http.MethodGet, path, nil, nil)
			results <- result{path: path, resp: resp, err: err}
		}(path)
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case r := <-results:
			if r.err != nil {
				t.Fatalf("%s: %v", r.path, r.err)
			}
			if !strings.Contains(string(r.resp.Body), r.path) {
				t.Fatalf("%s got %q", r.path, r.resp.Body)
			}
			if seen[r.path] {
				t.Fatalf("duplicate result for %s", r.path)
			}
			seen[r.path] = true
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent calls timed out")
		}
	}
}

func TestChatConnCloseUnblocksCall(t *testing.T) {
	ws := musetest.New(t)
	ws.SetStreamHandler(func(req musetest.Request, em *musetest.Emitter) musetest.Response {
		// Never respond: close must unblock the caller.
		return musetest.Response{Status: 200, NoEnd: true}
	})
	cc := dialChatTestConn(t, ws.URL)
	errCh := make(chan error, 1)
	go func() {
		_, err := cc.call(context.Background(), http.MethodGet, "/never", nil, nil)
		errCh <- err
	}()
	time.Sleep(30 * time.Millisecond)
	cc.close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("call succeeded after close")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call did not unblock on close")
	}
}

func TestBuildAppRequest(t *testing.T) {
	req := buildAppRequest(http.MethodPost, "/chat/stream", []byte(`{"a":1}`), nil)
	if req.Verb != http.MethodPost || !req.EndBody || len(req.Body) == 0 {
		t.Fatalf("request: %+v", req)
	}
	var gotID, gotCT bool
	for _, h := range req.Headers {
		if h.Key == "x-request-id" && h.Value != "" {
			gotID = true
		}
		if h.Key == "content-type" && h.Value == "application/json" {
			gotCT = true
		}
	}
	if !gotID || !gotCT {
		t.Fatalf("headers: %+v", req.Headers)
	}
	req = buildAppRequest(http.MethodGet, "/chat/history?limit=1", nil,
		[]noiseHeader{{Key: "x-request-id", Value: "fixed"}})
	if len(req.Body) != 0 || len(req.Headers) != 1 || req.Headers[0].Value != "fixed" {
		t.Fatalf("GET request: %+v", req)
	}
}

type atomicInt struct {
	mu sync.Mutex
	n  int
}

func (a *atomicInt) Add(d int) {
	a.mu.Lock()
	a.n += d
	a.mu.Unlock()
}

func (a *atomicInt) Load() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.n
}

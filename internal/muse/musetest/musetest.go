// Package musetest provides a loopback Muse Noise gateway for tests. It is
// intentionally independent of the production frame code: the mock decodes
// requests with its own small protobuf reader so a field-number bug cannot
// hide behind a shared encoder. It is only imported from _test.go files.
package musetest

import (
	"context"
	"encoding/binary"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/flynn/noise"
)

// Request is one decoded ApplicationRequest.
type Request struct {
	StreamID int64
	Verb     string
	Path     string
	Headers  map[string]string
	Body     []byte
}

// Reset makes the mock send a stream reset instead of an application response.
type Reset struct {
	Code   int
	Reason string
}

// Response is what the handler returns for one request.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	EndBody bool
	Reset   *Reset

	streamID int64
}

// Server is a loopback WSS/WS Noise gateway.
type Server struct {
	URL string // ws://host/v1/noise

	http *httptest.Server

	mu       sync.Mutex
	dials    int
	failN    int
	requests []Request
	handler  func(Request) Response
}

// New starts a mock Noise gateway and registers cleanup with t.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{}
	s.http = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.http.Close)
	s.URL = "ws" + strings.TrimPrefix(s.http.URL, "http") + "/v1/noise"
	return s
}

// SetHandler changes the response function. It is safe to call before serving;
// tests normally set it immediately after New.
func (s *Server) SetHandler(fn func(Request) Response) {
	s.mu.Lock()
	s.handler = fn
	s.mu.Unlock()
}

// FailFirstDials closes the first n accepted WebSockets before the Noise
// handshake. It exercises the dial budget without waiting for network timeouts.
func (s *Server) FailFirstDials(n int) {
	s.mu.Lock()
	s.failN = n
	s.mu.Unlock()
}

func (s *Server) Dials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dials
}

func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/noise" {
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.dials++
	fail := s.dials <= s.failN
	handler := s.handler
	s.mu.Unlock()

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	if fail {
		return
	}
	if handler == nil {
		handler = func(Request) Response {
			return Response{Status: 200, Body: []byte(`{"ok":true}`), EndBody: true}
		}
	}
	if err := s.serve(ws, handler); err != nil {
		return
	}
}

func (s *Server) serve(ws *websocket.Conn, handler func(Request) Response) error {
	ctx := context.Background()
	cs := noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)
	static, err := noise.DH25519.GenerateKeypair(nil)
	if err != nil {
		return err
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cs,
		Pattern:       noise.HandshakeXX,
		Initiator:     false,
		StaticKeypair: static,
	})
	if err != nil {
		return err
	}
	typ, m1, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		return errors.New("mock: read msg1")
	}
	if _, _, _, err := hs.ReadMessage(nil, m1); err != nil {
		return err
	}
	m2, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return err
	}
	if err := ws.Write(ctx, websocket.MessageBinary, m2); err != nil {
		return err
	}
	typ, m3, err := ws.Read(ctx)
	if err != nil || typ != websocket.MessageBinary {
		return errors.New("mock: read msg3")
	}
	_, recv, send, err := hs.ReadMessage(nil, m3)
	if err != nil {
		return err
	}
	if send == nil || recv == nil {
		return errors.New("mock: incomplete handshake")
	}

	var chunkID int64 = 1
	for {
		typ, ciphertext, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageBinary {
			return errors.New("mock: non-binary frame")
		}
		plain, err := recv.Decrypt(nil, nil, ciphertext)
		if err != nil {
			return err
		}
		payload, err := decodeTransportPayload(plain)
		if err != nil {
			return err
		}
		req, err := decodeRequest(payload)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()

		resp := handler(req)
		resp.streamID = req.StreamID
		respWire := encodeResponse(resp)
		frame := encodeTransportFrame(chunkID, respWire)
		chunkID++
		ct, err := send.Encrypt(nil, nil, frame)
		if err != nil {
			return err
		}
		if err := ws.Write(ctx, websocket.MessageBinary, ct); err != nil {
			return err
		}
	}
}

// ---- independent protobuf helpers -----------------------------------------

func uvarint(v uint64) []byte {
	var b []byte
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			c |= 0x80
		}
		b = append(b, c)
		if v == 0 {
			return b
		}
	}
}

func tag(field, wire int) []byte { return uvarint(uint64(field<<3 | wire)) }

func fieldBytes(field int, v []byte) []byte {
	out := append(tag(field, 2), uvarint(uint64(len(v)))...)
	return append(out, v...)
}

func fieldString(field int, v string) []byte { return fieldBytes(field, []byte(v)) }

func fieldUint(field int, v uint64) []byte {
	if v == 0 {
		return nil
	}
	return append(tag(field, 0), uvarint(v)...)
}

func fieldBool(field int, v bool) []byte {
	if !v {
		return nil
	}
	return append(tag(field, 0), 1)
}

type pbField struct {
	num  int
	wire int
	v    uint64
	b    []byte
}

func parseFields(data []byte) ([]pbField, error) {
	var out []pbField
	for len(data) > 0 {
		t, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, errors.New("mock: bad tag")
		}
		data = data[n:]
		f := pbField{num: int(t >> 3), wire: int(t & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, errors.New("mock: bad varint")
			}
			f.v, data = v, data[n:]
		case 1:
			if len(data) < 8 {
				return nil, errors.New("mock: short fixed64")
			}
			f.b, data = data[:8], data[8:]
		case 2:
			l, n := binary.Uvarint(data)
			if n <= 0 || uint64(len(data)-n) < l {
				return nil, errors.New("mock: short bytes")
			}
			f.b, data = data[n:n+int(l)], data[n+int(l):]
		case 5:
			if len(data) < 4 {
				return nil, errors.New("mock: short fixed32")
			}
			f.b, data = data[:4], data[4:]
		default:
			return nil, errors.New("mock: unsupported wire type")
		}
		out = append(out, f)
	}
	return out, nil
}

func decodeTransportPayload(raw []byte) ([]byte, error) {
	fields, err := parseFields(raw)
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		if f.num == 4 && f.wire == 2 {
			return f.b, nil
		}
	}
	return nil, errors.New("mock: transport frame has no payload")
}

func decodeRequest(raw []byte) (Request, error) {
	// ServiceRequest.payload = field 2.
	var frame []byte
	fields, err := parseFields(raw)
	if err != nil {
		return Request{}, err
	}
	for _, f := range fields {
		if f.num == 2 && f.wire == 2 {
			frame = f.b
		}
	}
	frameFields, err := parseFields(frame)
	if err != nil {
		return Request{}, err
	}
	req := Request{Headers: map[string]string{}}
	var appReq []byte
	for _, f := range frameFields {
		switch {
		case f.num == 1 && f.wire == 0:
			req.StreamID = int64(f.v)
		case f.num == 2 && f.wire == 2:
			appReq = f.b
		}
	}
	appFields, err := parseFields(appReq)
	if err != nil {
		return Request{}, err
	}
	for _, f := range appFields {
		switch f.num {
		case 1:
			req.Verb = string(f.b)
		case 2:
			req.Path = string(f.b)
		case 3:
			hf, err := parseFields(f.b)
			if err != nil {
				return Request{}, err
			}
			var k, v string
			for _, h := range hf {
				if h.num == 1 {
					k = string(h.b)
				}
				if h.num == 2 {
					v = string(h.b)
				}
			}
			if k != "" {
				req.Headers[k] = v
			}
		case 4:
			req.Body = append([]byte(nil), f.b...)
		}
	}
	return req, nil
}

func encodeResponse(r Response) []byte {
	var frame []byte
	frame = append(frame, fieldUint(1, uint64(r.streamID))...)
	if r.Reset != nil {
		var rr []byte
		rr = append(rr, fieldUint(1, uint64(uint32(r.Reset.Code)))...)
		rr = append(rr, fieldString(2, r.Reset.Reason)...)
		frame = append(frame, fieldBytes(5, rr)...)
	} else {
		status := r.Status
		if status == 0 {
			status = 200
		}
		var app []byte
		app = append(app, fieldUint(1, uint64(uint32(status)))...)
		for k, v := range r.Headers {
			var h []byte
			h = append(h, fieldString(1, k)...)
			h = append(h, fieldString(2, v)...)
			app = append(app, fieldBytes(2, h)...)
		}
		app = append(app, fieldBytes(3, r.Body)...)
		app = append(app, fieldBool(4, r.EndBody || len(r.Body) == 0)...)
		frame = append(frame, fieldBytes(3, app)...)
	}
	return fieldBytes(1, frame)
}

func encodeTransportFrame(chunkID int64, payload []byte) []byte {
	var out []byte
	out = append(out, fieldUint(1, uint64(chunkID))...)
	out = append(out, fieldUint(3, 1)...)
	out = append(out, fieldBytes(4, payload)...)
	return out
}

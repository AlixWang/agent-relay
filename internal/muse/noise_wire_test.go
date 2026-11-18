package muse

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/flynn/noise"
)

const testUnreadUUID = "11111111-2222-4333-8444-555555555555"

func testUnreadRequestFrame(streamID int64) *serviceFrame {
	return &serviceFrame{
		StreamID: streamID,
		Request: &appRequest{
			Verb:    "GET",
			Path:    "/chat/unread-count",
			Headers: []noiseHeader{{Key: "x-request-id", Value: testUnreadUUID}},
			EndBody: true,
		},
	}
}

func TestUnreadRequestEncodingGolden(t *testing.T) {
	// golden bytes were generated independently from the field table, with a
	// fixed UUID and chunk id. Keeping the whole nesting in one assertion makes
	// a wrong field number or a missing end_body obvious.
	const (
		wantHeader = "0a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d353535353535353535353535"
		wantAppReq = "0a0347455412122f636861742f756e726561642d636f756e741a340a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d3535353535353535353535352801"
		wantFrame  = "080112510a0347455412122f636861742f756e726561642d636f756e741a340a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d3535353535353535353535352801"
		wantReq    = "1255080112510a0347455412122f636861742f756e726561642d636f756e741a340a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d3535353535353535353535352801"
		wantNtf    = "08888e98a8c0e0808101180122571255080112510a0347455412122f636861742f756e726561642d636f756e741a340a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d3535353535353535353535352801"
	)
	f := testUnreadRequestFrame(1)
	if got := hex.EncodeToString(encodeHeader(f.Request.Headers[0])); got != wantHeader {
		t.Fatalf("header golden:\n got %s\nwant %s", got, wantHeader)
	}
	if got := hex.EncodeToString(encodeAppRequest(f.Request)); got != wantAppReq {
		t.Fatalf("ApplicationRequest golden:\n got %s\nwant %s", got, wantAppReq)
	}
	if got := hex.EncodeToString(encodeServiceFrame(f)); got != wantFrame {
		t.Fatalf("ServiceFrame golden:\n got %s\nwant %s", got, wantFrame)
	}
	req := encodeServiceRequest(0, f)
	if got := hex.EncodeToString(req); got != wantReq {
		t.Fatalf("ServiceRequest golden:\n got %s\nwant %s", got, wantReq)
	}
	ntf := &noiseTransportFrame{
		ChunkID:     0x0102030405060708,
		TotalChunks: 1,
		Payload:     req,
	}
	if got := hex.EncodeToString(encodeNoiseTransportFrame(ntf)); got != wantNtf {
		t.Fatalf("NoiseTransportFrame golden:\n got %s\nwant %s", got, wantNtf)
	}
}

func TestDecodeUnreadRequest(t *testing.T) {
	raw, err := hex.DecodeString("08888e98a8c0e0808101180122571255080112510a0347455412122f636861742f756e726561642d636f756e741a340a0c782d726571756573742d6964122431313131313131312d323232322d343333332d383434342d3535353535353535353535352801")
	if err != nil {
		t.Fatal(err)
	}
	ntf, err := decodeNoiseTransportFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ntf.ChunkID != 0x0102030405060708 || ntf.TotalChunks != 1 || ntf.ChunkIndex != 0 {
		t.Fatalf("frame metadata: %+v", ntf)
	}
	fields, err := parsePB(ntf.Payload)
	if err != nil {
		t.Fatal(err)
	}
	var frameRaw []byte
	for _, f := range fields {
		if f.num == 2 && f.wire == 2 {
			frameRaw = f.b
		}
	}
	sf, err := decodeServiceFrame(frameRaw)
	if err != nil {
		t.Fatal(err)
	}
	if sf.StreamID != 1 || sf.Request == nil {
		t.Fatalf("frame: %+v", sf)
	}
	if sf.Request.Verb != "GET" || sf.Request.Path != "/chat/unread-count" || !sf.Request.EndBody {
		t.Fatalf("request: %+v", sf.Request)
	}
	if len(sf.Request.Headers) != 1 || sf.Request.Headers[0].Key != "x-request-id" || sf.Request.Headers[0].Value != testUnreadUUID {
		t.Fatalf("headers: %+v", sf.Request.Headers)
	}
	if len(sf.Request.Body) != 0 {
		t.Fatalf("GET body should be omitted: %q", sf.Request.Body)
	}
}

func TestDecodeApplicationResponse(t *testing.T) {
	raw, err := hex.DecodeString("0a2a08011a2608c801120b0a06782d746573741201311a127b22756e726561645f636f756e74223a307d2001")
	if err != nil {
		t.Fatal(err)
	}
	sf, err := decodeServiceResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if sf.StreamID != 1 || sf.Response == nil {
		t.Fatalf("frame: %+v", sf)
	}
	r := sf.Response
	if r.Status != 200 || string(r.Body) != `{"unread_count":0}` || !r.EndBody {
		t.Fatalf("response: %+v", r)
	}
	if len(r.Headers) != 1 || r.Headers[0].Key != "x-test" || r.Headers[0].Value != "1" {
		t.Fatalf("headers: %+v", r.Headers)
	}
}

func TestDecodeBodyChunkAndReset(t *testing.T) {
	sf, err := decodeServiceFrame(mustHex(t, "080122070a036162631001"))
	if err != nil {
		t.Fatal(err)
	}
	if sf.BodyChunk == nil || string(sf.BodyChunk.Data) != "abc" || !sf.BodyChunk.EndBody {
		t.Fatalf("body chunk: %+v", sf)
	}
	sf, err = decodeServiceFrame(mustHex(t, "08012a08080712046e6f7065"))
	if err != nil {
		t.Fatal(err)
	}
	if sf.Reset == nil || sf.Reset.Code != 7 || sf.Reset.Reason != "nope" {
		t.Fatalf("reset: %+v", sf)
	}
}

func TestOneofLastMemberWins(t *testing.T) {
	// response then reset: protobuf keeps the last oneof member.
	response := encodeAppResponse(&appResponse{Status: 200, Body: []byte("x"), EndBody: true})
	reset := encodeStreamReset(&streamReset{Code: 1, Reason: "cancel"})
	frame := pbWriter{}
	frame = frame.uintField(1, 1)
	frame = frame.bytesField(3, response)
	frame = frame.bytesField(5, reset)
	sf, err := decodeServiceFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if sf.Response != nil || sf.Reset == nil || sf.Reset.Code != 1 {
		t.Fatalf("oneof was not replaced by the last member: %+v", sf)
	}
}

func TestUnknownProtobufFieldsAreSkipped(t *testing.T) {
	valid := encodeAppResponse(&appResponse{
		Status:  204,
		Headers: []noiseHeader{{Key: "a", Value: "b"}},
		Body:    []byte("ok"),
		EndBody: true,
	})
	var raw pbWriter
	raw = append(raw, valid...)
	raw = raw.tag(90, 0).varint(7) // wire 0
	raw = append(raw, raw.tag(91, 1)...)
	raw = append(raw, 1, 2, 3, 4, 5, 6, 7, 8) // wire 1 payload
	raw = raw.tag(92, 2).varint(3)            // wire 2
	raw = append(raw, 'x', 'y', 'z')
	raw = append(raw, raw.tag(93, 5)...)
	raw = append(raw, 9, 10, 11, 12) // wire 5 payload
	got, err := decodeAppResponse(raw)
	if err != nil {
		t.Fatalf("unknown fields must be skipped: %v", err)
	}
	if got.Status != 204 || string(got.Body) != "ok" || !got.EndBody {
		t.Fatalf("known fields: %+v", got)
	}
}

func TestTruncatedProtobufIsAnError(t *testing.T) {
	for name, raw := range map[string][]byte{
		"tag":         {0x80},
		"varint":      {0x08, 0x80},
		"length":      {0x12, 0x05, 'a', 'b'},
		"fixed64":     {0x09, 1, 2, 3},
		"fixed32":     {0x0d, 1, 2},
		"field zero":  {0x00, 0x01},
		"bad wire":    {0x0b},
		"wire 3/4":    {0x0c},
		"length huge": {0x12, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x7f},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parsePB(raw); err == nil {
				t.Fatalf("parsePB(%x) should fail", raw)
			}
		})
	}
}

func TestNegativeChunkIDRoundTrips(t *testing.T) {
	raw := encodeNoiseTransportFrame(&noiseTransportFrame{ChunkID: -1, TotalChunks: 1, Payload: []byte("x")})
	// int64 -1 is ten bytes on the wire, not a four-byte uint cast.
	if got := hex.EncodeToString(raw); got != "08ffffffffffffffffff011801220178" {
		t.Fatalf("negative chunk id: %s", got)
	}
	f, err := decodeNoiseTransportFrame(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.ChunkID != -1 {
		t.Fatalf("chunk id: %d", f.ChunkID)
	}
}

func TestChunkReassemblerInterleaved(t *testing.T) {
	var r chunkReassembler
	feed := func(id int64, idx, total uint32, payload string) ([]byte, bool) {
		t.Helper()
		out, done, err := r.Feed(&noiseTransportFrame{ChunkID: id, ChunkIndex: idx, TotalChunks: total, Payload: []byte(payload)})
		if err != nil {
			t.Fatalf("feed id=%d idx=%d: %v", id, idx, err)
		}
		return out, done
	}
	if _, done := feed(7, 2, 3, "C"); done {
		t.Fatal("incomplete message reported complete")
	}
	if _, done := feed(9, 1, 2, "b"); done {
		t.Fatal("other chunk id completed")
	}
	if _, done := feed(7, 0, 3, "A"); done {
		t.Fatal("incomplete message reported complete")
	}
	if _, done := feed(9, 0, 2, "a"); !done {
		t.Fatal("second id did not complete")
	}
	out, done := feed(7, 1, 3, "B")
	if !done || string(out) != "ABC" {
		t.Fatalf("joined: %q done=%v", out, done)
	}
	if len(r.pending) != 0 {
		t.Fatalf("pending assemblies not cleared: %+v", r.pending)
	}
}

func TestChunkReassemblerMissingChunkNeverCompletes(t *testing.T) {
	var r chunkReassembler
	for _, idx := range []uint32{0, 2} {
		out, complete, err := r.Feed(&noiseTransportFrame{ChunkID: 1, ChunkIndex: idx, TotalChunks: 3, Payload: []byte("x")})
		if err != nil {
			t.Fatal(err)
		}
		if complete || out != nil {
			t.Fatalf("missing chunk %d completed: %q", 1, out)
		}
	}
	if len(r.pending) != 1 || len(r.pending[1].parts) != 2 {
		t.Fatalf("incomplete assembly lost: %+v", r.pending)
	}
}

func TestChunkReassemblerRejectsBadFrames(t *testing.T) {
	var r chunkReassembler
	for _, f := range []*noiseTransportFrame{
		{TotalChunks: 1, ChunkIndex: 1},
		{TotalChunks: maxNoiseChunks + 1},
		{TotalChunks: 2, ChunkIndex: 2},
		{TotalChunks: 2, ChunkIndex: 0, Payload: make([]byte, maxNoiseFramePayload+1)},
	} {
		if _, _, err := r.Feed(f); err == nil {
			t.Fatalf("frame should fail: %+v", f)
		}
	}
	if _, _, err := r.Feed(&noiseTransportFrame{ChunkID: 1, TotalChunks: 2, ChunkIndex: 0, Payload: []byte("a")}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.Feed(&noiseTransportFrame{ChunkID: 1, TotalChunks: 3, ChunkIndex: 1, Payload: []byte("b")}); err == nil {
		t.Fatal("inconsistent total_chunks should fail")
	}
	r = chunkReassembler{}
	_, _, _ = r.Feed(&noiseTransportFrame{ChunkID: 2, TotalChunks: 2, ChunkIndex: 0, Payload: []byte("a")})
	if _, _, err := r.Feed(&noiseTransportFrame{ChunkID: 2, TotalChunks: 2, ChunkIndex: 0, Payload: []byte("a")}); err == nil {
		t.Fatal("duplicate chunk_index should fail")
	}
}

func fixedKeypair(seed byte) noise.DHKey {
	kp, err := noise.DH25519.GenerateKeypair(bytes.NewReader(bytes.Repeat([]byte{seed}, 64)))
	if err != nil {
		panic(err)
	}
	return kp
}

// TestNoiseXXFixedVector drives a complete XX exchange with fixed static keys
// and deterministic randomness. Noise is randomized in production, so live
// ciphertext can never be replayed; this deterministic transcript is the
// closest stable vector and catches nonce/pattern regressions.
func TestNoiseXXFixedVector(t *testing.T) {
	cs := noiseCipherSuite()
	if got := "Noise_XX_" + string(cs.Name()); got != "Noise_XX_25519_AESGCM_SHA256" {
		t.Fatalf("cipher suite name: %q", got)
	}
	initiator, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cs,
		Random:        bytes.NewReader(bytes.Repeat([]byte{0xA1}, 256)),
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		Prologue:      nil,
		StaticKeypair: fixedKeypair(0x11),
	})
	if err != nil {
		t.Fatal(err)
	}
	responder, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cs,
		Random:        bytes.NewReader(bytes.Repeat([]byte{0xB2}, 256)),
		Pattern:       noise.HandshakeXX,
		Initiator:     false,
		Prologue:      nil,
		StaticKeypair: fixedKeypair(0x22),
	})
	if err != nil {
		t.Fatal(err)
	}

	m1, _, _, err := initiator.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(m1) == 0 {
		t.Fatal("empty msg1")
	}
	if _, _, _, err := responder.ReadMessage(nil, m1); err != nil {
		t.Fatal(err)
	}
	m2, _, _, err := responder.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if payload, _, _, err := initiator.ReadMessage(nil, m2); err != nil {
		t.Fatal(err)
	} else if len(payload) != 0 {
		t.Fatalf("standard VM msg2 payload should be opaque/empty in this vector: %x", payload)
	}
	m3, iSend, iRecv, err := initiator.WriteMessage(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, rRecv, rSend, err := responder.ReadMessage(nil, m3)
	if err != nil {
		t.Fatal(err)
	}
	if iSend == nil || iRecv == nil || rSend == nil || rRecv == nil {
		t.Fatal("handshake cipher states missing")
	}
	_ = iRecv

	plain := []byte("unread request")
	ciphertext, err := iSend.Encrypt(nil, nil, plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, plain) {
		t.Fatal("ciphertext leaks plaintext")
	}
	if got, err := rRecv.Decrypt(nil, nil, ciphertext); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(got, plain) {
		t.Fatalf("responder send state decrypt mismatch: %q", got)
	}
	var transcript [32]byte
	h := sha256.New()
	h.Write(m1)
	h.Write(m2)
	h.Write(m3)
	copy(transcript[:], h.Sum(nil))
	// Filled in from a local run and pinned so a flynn/noise update that
	// changes the XX transcript fails loudly instead of silently changing the
	// wire format.
	if got := hex.EncodeToString(transcript[:]); got != "c2678cfa82f37832820b96b624ed44461163ece92ebe93ef030a69649cb40378" {
		t.Fatalf("XX transcript sha256=%s", got)
	}
}

func TestNoiseTamperedCiphertextFails(t *testing.T) {
	cs := noiseCipherSuite()
	initiator, _ := noise.NewHandshakeState(noise.Config{CipherSuite: cs, Pattern: noise.HandshakeXX, Initiator: true, StaticKeypair: fixedKeypair(0x31)})
	responder, _ := noise.NewHandshakeState(noise.Config{CipherSuite: cs, Pattern: noise.HandshakeXX, Initiator: false, StaticKeypair: fixedKeypair(0x32)})
	m1, _, _, _ := initiator.WriteMessage(nil, nil)
	_, _, _, _ = responder.ReadMessage(nil, m1)
	m2, _, _, _ := responder.WriteMessage(nil, nil)
	_, _, _, _ = initiator.ReadMessage(nil, m2)
	m3, iSend, iRecv, _ := initiator.WriteMessage(nil, nil)
	_, rRecv, rSend, _ := responder.ReadMessage(nil, m3)
	if iSend == nil || iRecv == nil || rSend == nil || rRecv == nil {
		t.Fatal("handshake did not complete")
	}
	ct, err := iSend.Encrypt(nil, nil, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)-1] ^= 0xff
	if _, err := rRecv.Decrypt(nil, nil, ct); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestLooksLikeRVChallenge(t *testing.T) {
	nonce := bytes.Repeat([]byte{0x42}, 32)
	rv := pbWriter{}
	rv = rv.uintField(1, 1) // attestation-ish
	rv = rv.uintField(2, 1) // state
	rv = rv.bytesField(3, nonce)
	if !looksLikeRVChallenge(rv) {
		t.Fatal("RV challenge not detected")
	}
	standard := pbWriter{}
	standard = standard.bytesField(1, []byte("attestation"))
	if looksLikeRVChallenge(standard) {
		t.Fatal("standard attestation blob misclassified as RV")
	}
	if looksLikeRVChallenge([]byte{0x80}) {
		t.Fatal("malformed payload should not be detected")
	}
}

func TestNoiseErrorNeverLeaksSecrets(t *testing.T) {
	e := &noiseError{
		Stage:   "auth",
		Class:   "401",
		Attempt: 2,
		Elapsed: 1500 * 1e6,
		Cause:   errors.New("token=SECRET cookie=hatch_sess%3DSECRET wss://hatch.metaaivm.com/v1/noise?auth_token=SECRET"),
	}
	msg := e.Error()
	if msg != "noise auth: 401 (attempt 2/3, 1.5s)" {
		t.Fatalf("message: %q", msg)
	}
	for _, bad := range []string{"SECRET", "hatch_sess", "auth_token", "wss://"} {
		if strings.Contains(msg, bad) {
			t.Fatalf("error leaked %q: %s", bad, msg)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

package muse

// This file is the hand-written protobuf layer for the Muse gateway. The
// repository deliberately does not generate stubs from the web bundle's
// desc0.bin/desc1.bin: for a single read-only RPC that would add a protoc build
// step and a generated-code maintenance burden. Field numbers below come from
// muse-protocol-go/hatch/wire.go and muse-cli's descriptor extraction; unknown
// fields are skipped so an upstream schema addition cannot break decoding.
//
// Field map (muse-protocol-go/docs/protocol.md, hatch/wire.go:10-21):
//
//	NoiseTransportFrame { int64 chunk_id=1; uint32 chunk_index=2; uint32 total_chunks=3; bytes payload=4; }
//	ServiceRequest      { ServiceType service=1; bytes payload=2; }
//	ServiceResponse     { bytes payload=1; }
//	ServiceFrame        { int64 stream_id=1; oneof { ApplicationRequest request=2;
//	                      ApplicationResponse response=3; BodyChunk body_chunk=4; Reset reset=5; } }
//	ApplicationRequest  { string verb=1; string path=2; repeated Header headers=3; bytes body=4; bool end_body=5; }
//	ApplicationResponse { int32 status=1; repeated Header headers=2; bytes body=3; bool end_body=4; }
//	BodyChunk           { bytes data=1; bool end_body=2; }
//	Header              { string key=1; string value=2; }
//	Reset               { int32 code=1; string reason=2; }
//
// Proto defaults are omitted on the wire, matching the generated protobuf
// encoder: service=0, chunk_index=0, false, and empty bodies do not get a tag.

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	maxNoiseFramePayload = 1 << 20 // one transport frame
	maxNoiseAssembly     = 4 << 20 // one reassembled Noise message
	maxNoiseChunks       = 256
	maxNoisePending      = 32 // abandoned message ids retained at once
)

type noiseHeader struct {
	Key   string
	Value string
}

type appRequest struct {
	Verb    string
	Path    string
	Headers []noiseHeader
	Body    []byte
	EndBody bool
}

type appResponse struct {
	Status  int
	Headers []noiseHeader
	Body    []byte
	EndBody bool
}

type bodyChunk struct {
	Data    []byte
	EndBody bool
}

type streamReset struct {
	Code   int
	Reason string
}

// serviceFrame is the decoded oneof payload. At most one of Request/Response/
// BodyChunk/Reset is non-nil.
type serviceFrame struct {
	StreamID  int64
	Request   *appRequest
	Response  *appResponse
	BodyChunk *bodyChunk
	Reset     *streamReset
}

type noiseTransportFrame struct {
	ChunkID     int64
	ChunkIndex  uint32
	TotalChunks uint32
	Payload     []byte
}

// ---- encoding -------------------------------------------------------------

type pbWriter []byte

func (b pbWriter) varint(v uint64) pbWriter {
	return binary.AppendUvarint(b, v)
}

func (b pbWriter) tag(field, wire int) pbWriter {
	return b.varint(uint64(field<<3 | wire))
}

func (b pbWriter) uintField(field int, v uint64) pbWriter {
	if v == 0 {
		return b
	}
	return b.tag(field, 0).varint(v)
}

func (b pbWriter) boolField(field int, v bool) pbWriter {
	if !v {
		return b
	}
	return b.uintField(field, 1)
}

func (b pbWriter) bytesField(field int, v []byte) pbWriter {
	if len(v) == 0 {
		return b
	}
	b = b.tag(field, 2).varint(uint64(len(v)))
	return append(b, v...)
}

func (b pbWriter) stringField(field int, v string) pbWriter {
	if v == "" {
		return b
	}
	return b.bytesField(field, []byte(v))
}

func encodeHeader(h noiseHeader) []byte {
	b := pbWriter{}
	b = b.stringField(1, h.Key)
	b = b.stringField(2, h.Value)
	return b
}

func encodeAppRequest(r *appRequest) []byte {
	if r == nil {
		return nil
	}
	b := pbWriter{}
	b = b.stringField(1, r.Verb)
	b = b.stringField(2, r.Path)
	for _, h := range r.Headers {
		b = b.bytesField(3, encodeHeader(h))
	}
	b = b.bytesField(4, r.Body)
	b = b.boolField(5, r.EndBody)
	return b
}

func encodeAppResponse(r *appResponse) []byte {
	if r == nil {
		return nil
	}
	b := pbWriter{}
	b = b.uintField(1, uint64(uint32(r.Status)))
	for _, h := range r.Headers {
		b = b.bytesField(2, encodeHeader(h))
	}
	b = b.bytesField(3, r.Body)
	b = b.boolField(4, r.EndBody)
	return b
}

func encodeBodyChunk(c *bodyChunk) []byte {
	if c == nil {
		return nil
	}
	b := pbWriter{}
	b = b.bytesField(1, c.Data)
	b = b.boolField(2, c.EndBody)
	return b
}

func encodeStreamReset(r *streamReset) []byte {
	if r == nil {
		return nil
	}
	b := pbWriter{}
	b = b.uintField(1, uint64(uint32(r.Code)))
	b = b.stringField(2, r.Reason)
	return b
}

func encodeServiceFrame(f *serviceFrame) []byte {
	if f == nil {
		return nil
	}
	b := pbWriter{}
	b = b.uintField(1, uint64(f.StreamID)) // negative stream ids become 10-byte varints
	switch {
	case f.Request != nil:
		b = b.bytesField(2, encodeAppRequest(f.Request))
	case f.Response != nil:
		b = b.bytesField(3, encodeAppResponse(f.Response))
	case f.BodyChunk != nil:
		b = b.bytesField(4, encodeBodyChunk(f.BodyChunk))
	case f.Reset != nil:
		b = b.bytesField(5, encodeStreamReset(f.Reset))
	}
	return b
}

// encodeServiceRequest builds the client→gateway envelope. service=0 (daemon)
// is the proto default and is omitted just like generated code would.
func encodeServiceRequest(service int, f *serviceFrame) []byte {
	b := pbWriter{}
	b = b.uintField(1, uint64(service))
	return b.bytesField(2, encodeServiceFrame(f))
}

// encodeServiceResponse is used by the mock server/tests for the gateway→
// client direction. Production code only decodes responses.
func encodeServiceResponse(f *serviceFrame) []byte {
	b := pbWriter{}
	return b.bytesField(1, encodeServiceFrame(f))
}

func encodeNoiseTransportFrame(f *noiseTransportFrame) []byte {
	if f == nil {
		return nil
	}
	b := pbWriter{}
	b = b.uintField(1, uint64(f.ChunkID))
	b = b.uintField(2, uint64(f.ChunkIndex))
	b = b.uintField(3, uint64(f.TotalChunks))
	b = b.bytesField(4, f.Payload)
	return b
}

// ---- decoding -------------------------------------------------------------

type pbField struct {
	num  int
	wire int
	v    uint64
	b    []byte
}

// parsePB splits a protobuf message into fields. Wire types 3/4 (deprecated
// groups) are rejected; 0/1/2/5 are accepted and unknown fields are simply
// ignored by callers.
func parsePB(data []byte) ([]pbField, error) {
	out := make([]pbField, 0, 8)
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, errors.New("protobuf: bad tag varint")
		}
		data = data[n:]
		num := int(tag >> 3)
		if num <= 0 {
			return nil, errors.New("protobuf: invalid field number 0")
		}
		f := pbField{num: num, wire: int(tag & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, fmt.Errorf("protobuf: field %d has a truncated varint", num)
			}
			f.v, data = v, data[n:]
		case 1:
			if len(data) < 8 {
				return nil, fmt.Errorf("protobuf: field %d has a truncated fixed64", num)
			}
			f.b, data = data[:8], data[8:]
		case 2:
			l, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, fmt.Errorf("protobuf: field %d has a bad length", num)
			}
			data = data[n:]
			if l > uint64(len(data)) {
				return nil, fmt.Errorf("protobuf: field %d length %d exceeds %d remaining bytes", num, l, len(data))
			}
			f.b, data = data[:int(l)], data[int(l):]
		case 5:
			if len(data) < 4 {
				return nil, fmt.Errorf("protobuf: field %d has a truncated fixed32", num)
			}
			f.b, data = data[:4], data[4:]
		default:
			return nil, fmt.Errorf("protobuf: field %d has unsupported wire type %d", num, f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}

func decodeHeader(raw []byte) (noiseHeader, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return noiseHeader{}, err
	}
	var h noiseHeader
	for _, f := range fields {
		switch f.num {
		case 1:
			if f.wire == 2 {
				h.Key = string(f.b)
			}
		case 2:
			if f.wire == 2 {
				h.Value = string(f.b)
			}
		}
	}
	return h, nil
}

func decodeAppRequest(raw []byte) (*appRequest, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	r := &appRequest{}
	for _, f := range fields {
		switch f.num {
		case 1:
			if f.wire == 2 {
				r.Verb = string(f.b)
			}
		case 2:
			if f.wire == 2 {
				r.Path = string(f.b)
			}
		case 3:
			if f.wire == 2 {
				h, err := decodeHeader(f.b)
				if err != nil {
					return nil, err
				}
				r.Headers = append(r.Headers, h)
			}
		case 4:
			if f.wire == 2 {
				r.Body = append([]byte(nil), f.b...)
			}
		case 5:
			if f.wire == 0 {
				r.EndBody = f.v != 0
			}
		}
	}
	return r, nil
}

func decodeAppResponse(raw []byte) (*appResponse, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	r := &appResponse{}
	for _, f := range fields {
		switch f.num {
		case 1:
			if f.wire == 0 {
				r.Status = int(int32(f.v))
			}
		case 2:
			if f.wire == 2 {
				h, err := decodeHeader(f.b)
				if err != nil {
					return nil, err
				}
				r.Headers = append(r.Headers, h)
			}
		case 3:
			if f.wire == 2 {
				r.Body = append([]byte(nil), f.b...)
			}
		case 4:
			if f.wire == 0 {
				r.EndBody = f.v != 0
			}
		}
	}
	return r, nil
}

func decodeBodyChunk(raw []byte) (*bodyChunk, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	c := &bodyChunk{}
	for _, f := range fields {
		switch f.num {
		case 1:
			if f.wire == 2 {
				c.Data = append([]byte(nil), f.b...)
			}
		case 2:
			if f.wire == 0 {
				c.EndBody = f.v != 0
			}
		}
	}
	return c, nil
}

func decodeStreamReset(raw []byte) (*streamReset, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	r := &streamReset{}
	for _, f := range fields {
		switch f.num {
		case 1:
			if f.wire == 0 {
				r.Code = int(int32(f.v))
			}
		case 2:
			if f.wire == 2 {
				r.Reason = string(f.b)
			}
		}
	}
	return r, nil
}

func decodeServiceFrame(raw []byte) (*serviceFrame, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	f := &serviceFrame{}
	for _, field := range fields {
		switch field.num {
		case 1:
			if field.wire == 0 {
				f.StreamID = int64(field.v)
			}
		case 2, 3, 4, 5:
			if field.wire != 2 {
				continue
			}
			// oneof: the last member on the wire wins, as in protobuf.
			f.Request, f.Response, f.BodyChunk, f.Reset = nil, nil, nil, nil
			switch field.num {
			case 2:
				v, err := decodeAppRequest(field.b)
				if err != nil {
					return nil, err
				}
				f.Request = v
			case 3:
				v, err := decodeAppResponse(field.b)
				if err != nil {
					return nil, err
				}
				f.Response = v
			case 4:
				v, err := decodeBodyChunk(field.b)
				if err != nil {
					return nil, err
				}
				f.BodyChunk = v
			case 5:
				v, err := decodeStreamReset(field.b)
				if err != nil {
					return nil, err
				}
				f.Reset = v
			}
		}
	}
	return f, nil
}

// decodeServiceResponse unwraps ServiceResponse.payload (field 1).
func decodeServiceResponse(raw []byte) (*serviceFrame, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	for _, f := range fields {
		if f.num == 1 && f.wire == 2 {
			return decodeServiceFrame(f.b)
		}
	}
	return nil, errors.New("ServiceResponse has no payload")
}

func decodeNoiseTransportFrame(raw []byte) (*noiseTransportFrame, error) {
	fields, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	f := &noiseTransportFrame{}
	for _, field := range fields {
		switch field.num {
		case 1:
			if field.wire == 0 {
				f.ChunkID = int64(field.v)
			}
		case 2:
			if field.wire == 0 {
				f.ChunkIndex = uint32(field.v)
			}
		case 3:
			if field.wire == 0 {
				f.TotalChunks = uint32(field.v)
			}
		case 4:
			if field.wire == 2 {
				f.Payload = append([]byte(nil), field.b...)
			}
		}
	}
	return f, nil
}

// ---- NoiseTransportFrame chunk reassembly ---------------------------------

type chunkAssembly struct {
	total int
	parts map[int][]byte
	size  int
}

// chunkReassembler joins NoiseTransportFrames sharing a chunk_id. Frames of
// different ids may interleave. Incomplete assemblies are simply never
// returned; a missing chunk cannot panic or produce a partial message.
type chunkReassembler struct {
	pending  map[int64]*chunkAssembly
	order    []int64
	buffered int
}

func (r *chunkReassembler) remove(id int64) {
	a := r.pending[id]
	if a == nil {
		return
	}
	r.buffered -= a.size
	delete(r.pending, id)
	for i, v := range r.order {
		if v == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Feed consumes one transport frame. It returns (payload, true, nil) only when
// a complete message is available. total_chunks=0 is treated as 1, matching
// muse-cli's `ntf.total_chunks or 1`.
func (r *chunkReassembler) Feed(f *noiseTransportFrame) ([]byte, bool, error) {
	if f == nil {
		return nil, false, errors.New("noise transport frame is nil")
	}
	total := int(f.TotalChunks)
	if total == 0 {
		total = 1
	}
	if total < 1 || total > maxNoiseChunks {
		return nil, false, fmt.Errorf("noise transport frame has invalid total_chunks=%d", total)
	}
	idx := int(f.ChunkIndex)
	if idx < 0 || idx >= total {
		return nil, false, fmt.Errorf("noise transport frame has chunk_index=%d for total_chunks=%d", idx, total)
	}
	if len(f.Payload) > maxNoiseFramePayload {
		return nil, false, fmt.Errorf("noise transport frame payload exceeds %d bytes", maxNoiseFramePayload)
	}
	if total == 1 {
		return append([]byte(nil), f.Payload...), true, nil
	}
	if r.pending == nil {
		r.pending = make(map[int64]*chunkAssembly)
	}
	a := r.pending[f.ChunkID]
	if a == nil {
		if len(r.pending) >= maxNoisePending {
			r.remove(r.order[0])
		}
		a = &chunkAssembly{total: total, parts: make(map[int][]byte, total)}
		r.pending[f.ChunkID] = a
		r.order = append(r.order, f.ChunkID)
	}
	if a.total != total {
		r.remove(f.ChunkID)
		return nil, false, fmt.Errorf("noise chunks for id %d disagree on total_chunks", f.ChunkID)
	}
	if _, dup := a.parts[idx]; dup {
		r.remove(f.ChunkID)
		return nil, false, fmt.Errorf("noise chunks for id %d repeat chunk_index %d", f.ChunkID, idx)
	}
	if a.size+len(f.Payload) > maxNoiseAssembly || r.buffered+len(f.Payload) > maxNoiseAssembly {
		r.remove(f.ChunkID)
		return nil, false, errors.New("noise chunk assembly exceeds the buffer limit")
	}
	a.parts[idx] = append([]byte(nil), f.Payload...)
	a.size += len(f.Payload)
	r.buffered += len(f.Payload)
	if len(a.parts) != total {
		return nil, false, nil
	}
	out := make([]byte, 0, a.size)
	for i := 0; i < total; i++ {
		out = append(out, a.parts[i]...)
	}
	r.remove(f.ChunkID)
	return out, true, nil
}

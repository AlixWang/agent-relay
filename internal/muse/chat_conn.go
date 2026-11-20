package muse

// chat_conn.go owns the one-reader/multi-stream multiplexer for a chat
// connection. Noise recv/send CipherStates are stateful: exactly one goroutine
// may read frames, and all writes go through noiseConn.sendServiceFrame (which
// serializes encrypt+write). Business callers only register a stream sink and
// wait on its channel.

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

const chatSinkBuffer = 256

type chatWireFrame struct {
	Response  *appResponse
	BodyChunk *bodyChunk
	Reset     *streamReset
	Err       error
}

type chatSink struct {
	streamID int64
	ch       chan chatWireFrame

	mu     sync.Mutex
	closed bool
}

func newChatSink(streamID int64) *chatSink {
	return &chatSink{streamID: streamID, ch: make(chan chatWireFrame, chatSinkBuffer)}
}

// deliver is deliberately non-blocking: the read loop must never stall on a
// slow business consumer. A full sink is failed rather than growing memory
// without bound.
func (s *chatSink) deliver(f chatWireFrame) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	select {
	case s.ch <- f:
		return true
	default:
		select {
		case <-s.ch:
		default:
		}
		select {
		case s.ch <- chatWireFrame{Err: newNoiseError("chat", "consumer_stalled", false, nil)}:
		default:
		}
		return false
	}
}

func (s *chatSink) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

type chatConn struct {
	parent *noiseConn

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	next atomic.Int64

	mu      sync.Mutex
	sinks   map[int64]*chatSink
	closed  bool
	readErr error
	orphans int

	closeOnce sync.Once
}

func newChatConn(ctx context.Context, parent *noiseConn) *chatConn {
	cctx, cancel := context.WithCancel(ctx)
	c := &chatConn{
		parent: parent,
		ctx:    cctx,
		cancel: cancel,
		done:   make(chan struct{}),
		sinks:  make(map[int64]*chatSink),
	}
	go c.readLoop()
	return c
}

func (c *chatConn) nextStreamID() int64 {
	return c.next.Add(1)
}

func (c *chatConn) register(sink *chatSink) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if c.readErr != nil {
			return c.readErr
		}
		return newNoiseError("chat", "connection_closed", true, nil)
	}
	c.sinks[sink.streamID] = sink
	return nil
}

func (c *chatConn) unregister(streamID int64, sink *chatSink) {
	c.mu.Lock()
	if cur := c.sinks[streamID]; cur == sink {
		delete(c.sinks, streamID)
	}
	c.mu.Unlock()
}

func (c *chatConn) orphanCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orphans
}

func (c *chatConn) readLoop() {
	defer close(c.done)
	for {
		sf, err := c.parent.readServiceFrame(c.ctx)
		if err != nil {
			c.failAll(err)
			return
		}
		c.mu.Lock()
		sink := c.sinks[sf.StreamID]
		if sink == nil {
			c.orphans++
			n := c.orphans
			c.mu.Unlock()
			if n == 100 {
				log.Printf("muse chat: stream orphan count reached %d (late/unknown frames dropped)", n)
			}
			continue
		}
		terminal := sf.Reset != nil ||
			(sf.Response != nil && sf.Response.EndBody) ||
			(sf.BodyChunk != nil && sf.BodyChunk.EndBody)
		if terminal {
			delete(c.sinks, sf.StreamID)
		}
		c.mu.Unlock()

		frame := chatWireFrame{Response: sf.Response, BodyChunk: sf.BodyChunk, Reset: sf.Reset}
		if !sink.deliver(frame) {
			// The sink was already closed (caller timed out) or overflowed.
			// It is no longer useful; unregistering is idempotent.
			c.unregister(sf.StreamID, sink)
		}
	}
}

func (c *chatConn) failAll(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	c.closed = true
	sinks := make([]*chatSink, 0, len(c.sinks))
	for id, s := range c.sinks {
		sinks = append(sinks, s)
		delete(c.sinks, id)
	}
	c.mu.Unlock()
	for _, s := range sinks {
		s.deliver(chatWireFrame{Err: err})
	}
}

// writeRequestService registers the sink before sending so a fast response
// cannot race the map insertion, then writes exactly one request frame on the
// requested service.
func (c *chatConn) writeRequestService(ctx context.Context, service int, sink *chatSink,
	verb, path string, body []byte, headers []noiseHeader) (writeAttempted bool, err error) {

	if err := c.register(sink); err != nil {
		return false, err
	}
	sf := &serviceFrame{StreamID: sink.streamID, Request: buildAppRequest(verb, path, body, headers)}
	writeAttempted, err = c.parent.sendServiceFrameWithService(ctx, service, sf)
	if err != nil {
		c.unregister(sink.streamID, sink)
		sink.close()
	}
	return writeAttempted, err
}

// call is a unary daemon (service 0) application request on a chat connection.
// It is safe while a chat.stream is open because all frames are dispatched by
// readLoop.
func (c *chatConn) call(ctx context.Context, verb, path string, body []byte,
	headers []noiseHeader) (*appResponse, error) {

	return c.callService(ctx, 0, verb, path, body, headers)
}

// callService is the same unary request on an explicit service. It is reserved
// for the hidden approval probe: production chat traffic stays on service 0.
func (c *chatConn) callService(ctx context.Context, service int, verb, path string, body []byte,
	headers []noiseHeader) (*appResponse, error) {

	sink := newChatSink(c.nextStreamID())
	defer func() {
		c.unregister(sink.streamID, sink)
		sink.close()
	}()
	if _, err := c.writeRequestService(ctx, service, sink, verb, path, body, headers); err != nil {
		return nil, err
	}
	var resp *appResponse
	for {
		select {
		case f := <-sink.ch:
			if f.Err != nil {
				return nil, f.Err
			}
			if f.Reset != nil {
				return nil, newNoiseError("chat", resetClass(f.Reset.Code), false, nil)
			}
			switch {
			case f.Response != nil:
				if resp != nil {
					return nil, newNoiseError("chat", "duplicate_response", false, nil)
				}
				resp = f.Response
				if resp.EndBody {
					return resp, nil
				}
			case f.BodyChunk != nil:
				if resp == nil {
					return nil, newNoiseError("chat", "chunk_before_response", false, nil)
				}
				resp.Body = append(resp.Body, f.BodyChunk.Data...)
				if len(resp.Body) > maxNoiseAssembly {
					return nil, newNoiseError("chat", "body_too_large", false, nil)
				}
				if f.BodyChunk.EndBody {
					return resp, nil
				}
			}
		case <-ctx.Done():
			return nil, newNoiseError("chat", noiseNetworkClass(ctx.Err()), true, ctx.Err())
		case <-c.ctx.Done():
			c.mu.Lock()
			err := c.readErr
			c.mu.Unlock()
			if err == nil {
				err = newNoiseError("chat", "connection_closed", true, c.ctx.Err())
			}
			return nil, err
		}
	}
}

// openStream registers a long-lived stream (chat.stream echo) and returns its
// sink immediately. The caller owns consumption and should unregister on any
// early exit; terminal frames already unregister themselves in readLoop.
func (c *chatConn) openStream(ctx context.Context, verb, path string, body []byte,
	headers []noiseHeader) (*chatSink, chatSendEvidence, error) {

	sink := newChatSink(c.nextStreamID())
	attempted, err := c.writeRequestService(ctx, 0, sink, verb, path, body, headers)
	if !attempted {
		return nil, chatSendNotAttempted, err
	}
	if err != nil {
		return nil, chatSendMayHaveArrived, err
	}
	return sink, chatSendMayHaveArrived, nil
}

func (c *chatConn) unregisterStream(s *chatSink) {
	if s == nil {
		return
	}
	c.unregister(s.streamID, s)
	s.close()
}

// close is idempotent and waits briefly for the reader to exit so tests and the
// job lifecycle can assert no goroutine still owns the Noise state.
func (c *chatConn) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
		c.cancel()
		c.parent.close()
		select {
		case <-c.done:
		case <-time.After(2 * time.Second):
			log.Printf("muse chat: connection read loop did not stop after close")
		}
	})
}

func resetClass(code int) string {
	switch code {
	case 4:
		return "reset 4"
	default:
		return "reset"
	}
}

func chatNoiseClass(err error) string {
	if err == nil {
		return ""
	}
	var ne *noiseError
	if errors.As(err, &ne) {
		return ne.Class
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	return "network"
}

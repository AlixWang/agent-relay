package muse

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/flynn/noise"
)

const (
	noiseGatewayDefault = "wss://hatch.metaaivm.com/v1/noise"
	noiseBrowserOrigin  = "https://muse.ai"
	noiseMaxAttempts    = 3
	noiseReadLimit      = 1 << 20
)

// noiseError is the safe, loggable failure type for the Noise verification.
// Its Error() intentionally never includes the WSS URL, auth token, cookie
// values, Authorization header or response body: only a stage and a short
// class/status. The wrapped cause is used for errors.Is/As (for example to
// recover Retry-After from a 429) but is not rendered.
type noiseError struct {
	Stage     string // auth | token | dial | handshake | rpc
	Class     string // timeout | 401 | bad_decrypt | ...
	Retryable bool
	Attempt   int
	Elapsed   time.Duration
	Cause     error
}

func (e *noiseError) Error() string {
	class := strings.TrimSpace(e.Class)
	if class == "" {
		class = "error"
	}
	if e.Attempt > 0 {
		return fmt.Sprintf("noise %s: %s (attempt %d/%d, %.1fs)",
			e.Stage, class, e.Attempt, noiseMaxAttempts, e.Elapsed.Seconds())
	}
	return fmt.Sprintf("noise %s: %s", e.Stage, class)
}

func (e *noiseError) Unwrap() error { return e.Cause }

func newNoiseError(stage, class string, retryable bool, cause error) *noiseError {
	return &noiseError{Stage: stage, Class: class, Retryable: retryable, Cause: cause}
}

func noiseStatusClass(code int) string { return strconv.Itoa(code) }

func noiseNetworkClass(err error) string {
	if err == nil {
		return "network"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "network"
}

// noiseConn is one Noise-XX-encrypted WebSocket. One reader and one writer are
// expected at a time; the write mutex is a backstop because the Noise send
// nonce is stateful.
type noiseConn struct {
	ws    *websocket.Conn
	send  *noise.CipherState
	recv  *noise.CipherState
	reasm chunkReassembler

	writeMu sync.Mutex
}

func (c *noiseConn) close() {
	if c == nil || c.ws == nil {
		return
	}
	_ = c.ws.Close(websocket.StatusNormalClosure, "")
}

func noiseCipherSuite() noise.CipherSuite {
	return noise.NewCipherSuite(noise.DH25519, noise.CipherAESGCM, noise.HashSHA256)
}

// dialNoiseConn opens the shared hatch WSS endpoint and completes a standard
// Noise_XX_25519_AESGCM_SHA256 handshake as initiator with an empty prologue
// and no PSK. auth_token travels in the query string, never in the handshake
// payload. The static key is generated per connection and not persisted.
func dialNoiseConn(ctx context.Context, client *http.Client, gatewayURL, vmID, authToken string) (*noiseConn, error) {
	u, err := url.Parse(gatewayURL)
	if err != nil || u.Host == "" {
		return nil, newNoiseError("dial", "bad_gateway_url", false, nil)
	}
	// URL.Query() copies; never mutate the parsed service value.
	q := u.Query()
	q.Set("vm_id", vmID)
	q.Set("auth_token", authToken)
	u.RawQuery = q.Encode()

	header := http.Header{}
	header.Set("Origin", noiseBrowserOrigin)
	header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/135.0.0.0 Safari/537.36")
	header.Set("Accept-Language", "en-US,en;q=0.9")
	header.Set("Cache-Control", "no-cache")
	header.Set("Pragma", "no-cache")

	// The service client's 12s Timeout is for ordinary HTTPS calls. Noise has
	// its own per-phase deadlines and a 25s whole-verification budget.
	wsClient := *client
	wsClient.Timeout = 0
	opts := &websocket.DialOptions{HTTPClient: &wsClient, HTTPHeader: header}
	ws, resp, err := websocket.Dial(ctx, u.String(), opts)
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, newNoiseError("dial", noiseStatusClass(resp.StatusCode), false, err)
		}
		return nil, newNoiseError("dial", noiseNetworkClass(err), true, err)
	}
	ws.SetReadLimit(noiseReadLimit)
	return &noiseConn{ws: ws}, nil
}

// handshake runs the three XX messages. msg1 and msg3 carry empty payloads,
// matching the muse-cli path that was verified live. A confidential-VM (RV)
// challenge is reported as unsupported rather than silently downgraded.
func (c *noiseConn) handshake(ctx context.Context, timeout time.Duration) error {
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cs := noiseCipherSuite()
	static, err := noise.DH25519.GenerateKeypair(rand.Reader)
	if err != nil {
		return newNoiseError("handshake", "keygen", false, err)
	}
	hs, err := noise.NewHandshakeState(noise.Config{
		CipherSuite:   cs,
		Pattern:       noise.HandshakeXX,
		Initiator:     true,
		Prologue:      nil,
		StaticKeypair: static,
	})
	if err != nil {
		return newNoiseError("handshake", "init", false, err)
	}
	// UNVERIFIED: muse-cli sends an empty msg1 payload. muse-protocol-go sends
	// a 32-byte client_nonce; if the live check sees an immediate close after
	// msg1, that variant is the first thing to try.
	m1, _, _, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return newNoiseError("handshake", "write1", false, err)
	}
	if err := c.write(ctx, m1); err != nil {
		return err
	}
	typ, m2, err := c.ws.Read(hctx)
	if err != nil {
		return newNoiseError("handshake", noiseNetworkClass(err), true, err)
	}
	if typ != websocket.MessageBinary {
		return newNoiseError("handshake", "non_binary", false, nil)
	}
	payload, _, _, err := hs.ReadMessage(nil, m2)
	if err != nil {
		class := "handshake"
		if strings.Contains(strings.ToLower(err.Error()), "decrypt") {
			class = "bad_decrypt"
		}
		return newNoiseError("handshake", class, true, err)
	}
	if looksLikeRVChallenge(payload) {
		return newNoiseError("handshake", "rv_unsupported", false, nil)
	}
	m3, send, recv, err := hs.WriteMessage(nil, nil)
	if err != nil {
		return newNoiseError("handshake", "write3", false, err)
	}
	if err := c.write(ctx, m3); err != nil {
		return err
	}
	if send == nil || recv == nil {
		return newNoiseError("handshake", "incomplete", false, nil)
	}
	c.send, c.recv = send, recv
	return nil
}

// looksLikeRVChallenge detects hatch.noise.NoiseHandshakeMessage2 fields
// state=2 (1 or 2) and vm_nonce=3 (32 bytes). Standard VMs may carry an
// attestation blob, but not the state/nonce pair.
func looksLikeRVChallenge(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	fields, err := parsePB(payload)
	if err != nil {
		return false
	}
	var state, nonce bool
	for _, f := range fields {
		if f.num == 2 && f.wire == 0 && (f.v == 1 || f.v == 2) {
			state = true
		}
		if f.num == 3 && f.wire == 2 && len(f.b) == 32 {
			nonce = true
		}
	}
	return state && nonce
}

func (c *noiseConn) write(ctx context.Context, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageBinary, payload); err != nil {
		return newNoiseError("dial", noiseNetworkClass(err), true, err)
	}
	return nil
}

// encryptFrame wraps a plaintext service payload in a NoiseTransportFrame with
// a fresh random chunk_id, encrypts it and sends one binary WS message.
func (c *noiseConn) encryptFrame(payload []byte) ([]byte, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, newNoiseError("rpc", "rand", false, err)
	}
	frame := &noiseTransportFrame{
		ChunkID:     int64(binary.LittleEndian.Uint64(raw[:])),
		ChunkIndex:  0,
		TotalChunks: 1,
		Payload:     payload,
	}
	plain := encodeNoiseTransportFrame(frame)
	out, err := c.send.Encrypt(nil, nil, plain)
	if err != nil {
		return nil, newNoiseError("rpc", "encrypt", false, err)
	}
	return out, nil
}

// readServiceFrame reads WS messages, decrypts each transport frame and joins
// chunks until one complete ServiceResponse is available.
func (c *noiseConn) readServiceFrame(ctx context.Context) (*serviceFrame, error) {
	for i := 0; i < maxNoiseChunks; i++ {
		typ, ciphertext, err := c.ws.Read(ctx)
		if err != nil {
			return nil, newNoiseError("rpc", noiseNetworkClass(err), true, err)
		}
		if typ != websocket.MessageBinary {
			return nil, newNoiseError("rpc", "non_binary", true, nil)
		}
		plain, err := c.recv.Decrypt(nil, nil, ciphertext)
		if err != nil {
			return nil, newNoiseError("rpc", "bad_decrypt", true, err)
		}
		frame, err := decodeNoiseTransportFrame(plain)
		if err != nil {
			return nil, newNoiseError("rpc", "bad_frame", true, err)
		}
		payload, complete, err := c.reasm.Feed(frame)
		if err != nil {
			return nil, newNoiseError("rpc", "bad_chunk", true, err)
		}
		if !complete {
			continue
		}
		sf, err := decodeServiceResponse(payload)
		if err != nil {
			return nil, newNoiseError("rpc", "bad_response", true, err)
		}
		return sf, nil
	}
	return nil, newNoiseError("rpc", "too_many_frames", true, nil)
}

// doRPC sends one unary GET and collects its response. Non-2xx responses are
// returned to the caller (the unread→identity fallback needs the status);
// only transport/reset failures produce an error. The caller owns a per-RPC
// context; this method never retries on the same stream.
func (c *noiseConn) doRPC(ctx context.Context, streamID int64, path string) (*appResponse, error) {
	req := &serviceFrame{
		StreamID: streamID,
		Request: &appRequest{
			Verb:    http.MethodGet,
			Path:    path,
			Headers: []noiseHeader{{Key: "x-request-id", Value: newUUID()}},
			EndBody: true,
		},
	}
	envelope := encodeServiceRequest(0, req) // service 0 = daemon
	wire, err := c.encryptFrame(envelope)
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	err = c.ws.Write(ctx, websocket.MessageBinary, wire)
	c.writeMu.Unlock()
	if err != nil {
		return nil, newNoiseError("rpc", noiseNetworkClass(err), true, err)
	}

	var resp *appResponse
	for i := 0; i < maxNoiseChunks; i++ {
		sf, err := c.readServiceFrame(ctx)
		if err != nil {
			return nil, err
		}
		if sf.StreamID != streamID {
			continue // a future multiplexed call may interleave streams
		}
		switch {
		case sf.Reset != nil:
			return nil, newNoiseError("rpc", fmt.Sprintf("reset %d", sf.Reset.Code), false, nil)
		case sf.Response != nil:
			resp = sf.Response
			if resp.EndBody {
				return resp, nil
			}
		case sf.BodyChunk != nil && resp != nil:
			resp.Body = append(resp.Body, sf.BodyChunk.Data...)
			if len(resp.Body) > maxNoiseAssembly {
				return nil, newNoiseError("rpc", "body_too_large", false, nil)
			}
			if sf.BodyChunk.EndBody {
				return resp, nil
			}
		}
	}
	return nil, newNoiseError("rpc", "too_many_frames", true, nil)
}

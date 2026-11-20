package muse

import "context"

// chatFailure is the safe, loggable error for the direct-chat path. It carries
// only a stage/class and optional HTTP status/Retry-After; upstream bodies,
// tokens, cookies and WSS URLs never enter Error().
type chatFailure struct {
	Code       string
	Stage      string
	Retryable  bool
	RetryAfter int
	Status     int
	Cause      error
}

func (e *chatFailure) Error() string {
	if e == nil {
		return "chat error"
	}
	if e.Code != "" {
		return e.Code
	}
	if e.Stage != "" {
		return "chat " + e.Stage + " failed"
	}
	return "chat failed"
}

func (e *chatFailure) Unwrap() error { return e.Cause }

func newChatFailure(code, stage string, retryable bool) *chatFailure {
	return &chatFailure{Code: code, Stage: stage, Retryable: retryable}
}

func chatFailureAs(err error) *chatFailure {
	if err == nil {
		return nil
	}
	var cf *chatFailure
	if ok := errorsAs(err, &cf); ok {
		return cf
	}
	return &chatFailure{Code: "muse_upstream_error", Stage: "chat", Retryable: true, Cause: err}
}

// errorsAs keeps this file independent from errors just for one call site.
func errorsAs(err error, target **chatFailure) bool {
	for err != nil {
		if cf, ok := err.(*chatFailure); ok {
			*target = cf
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

type chatSendEvidence uint8

const (
	chatSendNotAttempted chatSendEvidence = iota
	chatSendMayHaveArrived
	chatSendRejected
)

type chatSendResult struct {
	Evidence    chatSendEvidence
	Status      int
	Echo        bool
	EchoLines   int
	TimedOut    bool
	InvalidEcho bool
}

type chatCancelResult struct {
	Sent        bool
	Unsupported bool
	Status      int
}

// chatSession is one job's dedicated connection. History/Cancel may run while a
// chat.stream sink is open; the chatConn single-reader dispatcher keeps the
// Noise receive state consistent.
type chatSession interface {
	History(ctx context.Context, sessionID string, limit int, beforeSeq int64) (*chatHistoryResponse, error)
	Send(ctx context.Context, sessionID, text, nodeID string) (chatSendResult, error)
	Cancel(ctx context.Context, sessionID string) (chatCancelResult, error)
	Close()
}

type chatGateway interface {
	Open(ctx context.Context, accountID string) (chatSession, error)
}

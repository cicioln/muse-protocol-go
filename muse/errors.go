package muse

import (
	"errors"
	"fmt"
	"time"
)

type Kind int

const (
	KindTransient  Kind = iota // retry, possibly on another account
	KindAuth                   // session rejected: account needs new cookies
	KindQuota                  // account out of credits
	KindRefusal                // model answered with text instead of media
	KindBadRequest             // caller's input is invalid; never retry
	KindRateLimit              // temporary upstream throttling, distinct from exhausted credits
)

type Error struct {
	Kind       Kind
	Msg        string
	Err        error
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

func errf(k Kind, format string, a ...any) *Error {
	return &Error{Kind: k, Msg: fmt.Sprintf(format, a...)}
}

func wrap(k Kind, msg string, err error) *Error { return &Error{Kind: k, Msg: msg, Err: err} }

// KindOf classifies any error; unknown errors are transient.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindTransient
}

func BadRequest(format string, a ...any) *Error { return errf(KindBadRequest, format, a...) }

// NoRetry marks an operation whose side effects may already have occurred.
// The underlying error still determines account health and API status.
type NoRetry struct{ Err error }

// StageError identifies the failed phase without parsing translated messages.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

func (e *NoRetry) Error() string { return e.Err.Error() }
func (e *NoRetry) Unwrap() error { return e.Err }

// exhaustedToolError marks an upstream tool failure that already exhausted its
// own retries, distinguishing it from transport/polling NoRetry errors so long
// video segments do not blindly re-run hopeless tool calls.
type exhaustedToolError struct{ Err error }

func (e *exhaustedToolError) Error() string { return e.Err.Error() }
func (e *exhaustedToolError) Unwrap() error { return e.Err }

func Retryable(err error) bool {
	var n *NoRetry
	if errors.As(err, &n) {
		return false
	}
	return KindOf(err) != KindBadRequest && KindOf(err) != KindRefusal
}
func RetryAfterOf(err error) time.Duration {
	var e *Error
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

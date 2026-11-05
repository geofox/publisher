package destination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrorClass says what a failure means for retrying.
type ErrorClass string

const (
	// ClassTransient: nothing was applied; retrying later should work
	// (network unreachable, 5xx).
	ClassTransient ErrorClass = "transient"
	// ClassRateLimited: the server asked us to slow down (429).
	ClassRateLimited ErrorClass = "rate_limited"
	// ClassAuth: credentials are missing, expired or revoked (401/403).
	// Retrying won't help until the account is fixed.
	ClassAuth ErrorClass = "auth"
	// ClassInvalid: the request itself is wrong (400/413/422, media that
	// can't be fitted, a video over the cap). Retrying the same thing fails
	// the same way.
	ClassInvalid ErrorClass = "invalid"
	// ClassRejected: the server refused the content for another reason
	// (other 4xx).
	ClassRejected ErrorClass = "rejected"
	// ClassUnknownOutcome: the request that publishes may have been applied
	// (a timeout or a dropped connection after it was sent, a gateway
	// timeout). Retrying can duplicate unless the destination is idempotent.
	// Only an error marked with Committing can have this class: a timeout on
	// any other step (a login, an upload, a status poll) posted nothing.
	ClassUnknownOutcome ErrorClass = "unknown_outcome"
)

// Retryable reports whether an automatic retry is reasonable for the class,
// given how the destination handles repeats.
func (c ErrorClass) Retryable(mode IdempotencyMode) bool {
	switch c {
	case ClassTransient, ClassRateLimited, "":
		return true
	case ClassUnknownOutcome:
		return mode == IdempotentKey || mode == IdempotentNatural
	default:
		return false
	}
}

// Error is a classified publish failure.
type Error struct {
	Class ErrorClass
	// RetryAfter is the server's requested wait, for ClassRateLimited.
	RetryAfter time.Duration
	Err        error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return string(e.Class)
	}
	return e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Invalid wraps err as ClassInvalid: a request that will fail again unchanged.
func Invalid(err error) *Error { return &Error{Class: ClassInvalid, Err: err} }

// Invalidf is Invalid(fmt.Errorf(...)).
func Invalidf(format string, args ...any) *Error {
	return Invalid(fmt.Errorf(format, args...))
}

// HTTPError is a non-2xx response from a destination's API. Clients return
// it (wrapped is fine) so Classify can read the status.
type HTTPError struct {
	Op         string // what was being done ("createRecord")
	StatusCode int
	Header     http.Header
	Body       string // truncated response body
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("%s returned %d", e.Op, e.StatusCode)
	if b := strings.TrimSpace(e.Body); b != "" {
		msg += ": " + b
	}
	return msg
}

// NewHTTPError builds an HTTPError from a response, reading at most 2 KiB of
// its body. The caller still closes the body.
func NewHTTPError(op string, resp *http.Response) *HTTPError {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return &HTTPError{Op: op, StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: string(b)}
}

// commitErr marks an error from the request that creates the item.
type commitErr struct{ err error }

func (e *commitErr) Error() string { return e.err.Error() }
func (e *commitErr) Unwrap() error { return e.err }

// Committing marks err as coming from the request that publishes the item
// (createRecord, threads_publish, POST /statuses), so Classify may read a
// timeout or a dropped connection as an unknown outcome. A nil err gives nil.
// Callers check ctx.Err() before sending: a context already done sent
// nothing, and that error must stay unmarked.
func Committing(err error) error {
	if err == nil {
		return nil
	}
	return &commitErr{err: err}
}

// Classify turns any error into an *Error. A nil error gives nil; an *Error
// anywhere in the chain is returned as is.
func Classify(err error) *Error {
	if err == nil {
		return nil
	}
	var de *Error
	if errors.As(err, &de) {
		return de
	}
	var ce *commitErr
	committing := errors.As(err, &ce)
	var he *HTTPError
	if errors.As(err, &he) {
		return &Error{Class: classForCommit(ClassForStatus(he.StatusCode), he.StatusCode, committing),
			RetryAfter: retryAfter(he.Header), Err: err}
	}
	var sc interface{ HTTPStatus() int }
	if errors.As(err, &sc) {
		return &Error{Class: classForCommit(ClassForStatus(sc.HTTPStatus()), sc.HTTPStatus(), committing), Err: err}
	}
	return &Error{Class: classifyTransport(err, committing), Err: err}
}

// classForCommit refines a status class for the publishing request: a 504
// means a gateway gave up waiting while the server may still have applied it.
func classForCommit(c ErrorClass, code int, committing bool) ErrorClass {
	if committing && code == http.StatusGatewayTimeout {
		return ClassUnknownOutcome
	}
	return c
}

// ClassOf is Classify(err).Class, or "" for a nil error.
func ClassOf(err error) ErrorClass {
	if c := Classify(err); c != nil {
		return c.Class
	}
	return ""
}

// ClassForStatus maps an HTTP status to a class.
func ClassForStatus(code int) ErrorClass {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ClassAuth
	case code == http.StatusTooManyRequests:
		return ClassRateLimited
	case code == http.StatusRequestTimeout:
		return ClassTransient
	case code == http.StatusBadRequest || code == http.StatusRequestEntityTooLarge ||
		code == http.StatusUnsupportedMediaType || code == http.StatusUnprocessableEntity:
		return ClassInvalid
	case code >= 400 && code < 500:
		return ClassRejected
	default:
		return ClassTransient
	}
}

// classifyTransport sorts errors that never produced a response. A failure to
// connect sent nothing (transient). A timeout or a dropped connection on the
// publishing request may have happened after the server applied it (unknown
// outcome); on any other step nothing was posted yet (transient).
func classifyTransport(err error, committing bool) ErrorClass {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return ClassTransient
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return ClassTransient
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return ClassTransient
	}
	if !committing {
		return ClassTransient
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return ClassUnknownOutcome
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ClassUnknownOutcome
	}
	// Anything else (local logic, an unrecognized library error) keeps the
	// pre-classification behaviour: retry.
	return ClassTransient
}

// retryAfter parses a Retry-After header (seconds or HTTP date) or the
// RateLimit-Reset epoch-seconds header the atproto PDS sends.
func retryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && s >= 0 {
			return time.Duration(s) * time.Second
		}
		if t, err := http.ParseTime(v); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	if v := h.Get("Ratelimit-Reset"); v != "" {
		if s, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
			if d := time.Until(time.Unix(s, 0)); d > 0 {
				return d
			}
		}
	}
	return 0
}

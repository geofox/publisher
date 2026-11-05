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
	// ClassUnknownOutcome: the request may have been applied (a timeout or a
	// dropped connection after it was sent). Retrying can duplicate unless
	// the destination is idempotent.
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
	var he *HTTPError
	if errors.As(err, &he) {
		return &Error{Class: ClassForStatus(he.StatusCode), RetryAfter: retryAfter(he.Header), Err: err}
	}
	var sc interface{ HTTPStatus() int }
	if errors.As(err, &sc) {
		return &Error{Class: ClassForStatus(sc.HTTPStatus()), Err: err}
	}
	return &Error{Class: classifyTransport(err), Err: err}
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
// connect sent nothing (transient); a timeout or a dropped connection may have
// happened after the server applied the request (unknown outcome).
func classifyTransport(err error) ErrorClass {
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

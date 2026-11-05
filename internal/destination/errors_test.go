package destination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

type statusErr int

func (s statusErr) Error() string   { return "api error" }
func (s statusErr) HTTPStatus() int { return int(s) }

func TestClassify(t *testing.T) {
	he := func(code int) error {
		return fmt.Errorf("createRecord: %w", &HTTPError{Op: "x", StatusCode: code})
	}
	cases := []struct {
		name string
		err  error
		want ErrorClass
	}{
		{"400", he(400), ClassInvalid},
		{"413", he(413), ClassInvalid},
		{"422", he(422), ClassInvalid},
		{"401", he(401), ClassAuth},
		{"403", he(403), ClassAuth},
		{"404", he(404), ClassRejected},
		{"408", he(408), ClassTransient},
		{"429", he(429), ClassRateLimited},
		{"500", he(500), ClassTransient},
		{"503", he(503), ClassTransient},
		{"library status", fmt.Errorf("post: %w", statusErr(401)), ClassAuth},
		{"dial refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, ClassTransient},
		{"dns", &net.DNSError{Err: "no such host"}, ClassTransient},
		{"deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), ClassUnknownOutcome},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, ClassUnknownOutcome},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, ClassUnknownOutcome},
		{"eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), ClassUnknownOutcome},
		{"explicit invalid", fmt.Errorf("wrap: %w", Invalidf("video too long")), ClassInvalid},
		{"plain", errors.New("something odd"), ClassTransient},
	}
	for _, tc := range cases {
		if got := ClassOf(tc.err); got != tc.want {
			t.Errorf("%s: class = %q, want %q", tc.name, got, tc.want)
		}
	}
	if ClassOf(nil) != "" || Classify(nil) != nil {
		t.Error("nil error must classify to nil")
	}
}

func TestClassifyKeepsMessage(t *testing.T) {
	err := fmt.Errorf("createRecord: %w", &HTTPError{Op: "createRecord", StatusCode: 400, Body: "InvalidRecord"})
	if got := Classify(err).Error(); got != "createRecord: createRecord returned 400: InvalidRecord" {
		t.Errorf("message = %q", got)
	}
}

func TestRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "120")
	e := Classify(&HTTPError{StatusCode: 429, Header: h})
	if e.Class != ClassRateLimited || e.RetryAfter != 2*time.Minute {
		t.Errorf("got %+v", e)
	}
	h = http.Header{}
	h.Set("RateLimit-Reset", fmt.Sprint(time.Now().Add(90*time.Second).Unix()))
	e = Classify(&HTTPError{StatusCode: 429, Header: h})
	if e.RetryAfter < 80*time.Second || e.RetryAfter > 91*time.Second {
		t.Errorf("RateLimit-Reset → %v", e.RetryAfter)
	}
}

func TestRetryable(t *testing.T) {
	if !ClassTransient.Retryable(IdempotentNone) || !ClassRateLimited.Retryable(IdempotentNone) {
		t.Error("transient/rate_limited must retry")
	}
	for _, c := range []ErrorClass{ClassAuth, ClassInvalid, ClassRejected} {
		if c.Retryable(IdempotentKey) {
			t.Errorf("%s must not auto-retry", c)
		}
	}
	if ClassUnknownOutcome.Retryable(IdempotentNone) {
		t.Error("unknown outcome on a non-idempotent destination must not auto-retry")
	}
	if !ClassUnknownOutcome.Retryable(IdempotentKey) || !ClassUnknownOutcome.Retryable(IdempotentNatural) {
		t.Error("unknown outcome on an idempotent destination may retry")
	}
}

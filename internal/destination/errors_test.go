package destination

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
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
		// On the publishing request, a timeout or a dropped connection may
		// have been applied.
		{"commit deadline", Committing(fmt.Errorf("post: %w", context.DeadlineExceeded)), ClassUnknownOutcome},
		{"commit canceled", fmt.Errorf("x: %w", Committing(context.Canceled)), ClassUnknownOutcome},
		{"commit net timeout", Committing(&net.OpError{Op: "read", Err: timeoutErr{}}), ClassUnknownOutcome},
		{"commit reset", Committing(&net.OpError{Op: "read", Err: syscall.ECONNRESET}), ClassUnknownOutcome},
		{"commit eof", Committing(fmt.Errorf("read: %w", io.ErrUnexpectedEOF)), ClassUnknownOutcome},
		{"commit 504", Committing(he(504)), ClassUnknownOutcome},
		{"commit 500", Committing(he(500)), ClassTransient},
		{"commit 502", Committing(he(502)), ClassUnknownOutcome},
		{"commit 503", Committing(he(503)), ClassTransient},
		{"commit 422", Committing(he(422)), ClassInvalid},
		{"commit dial", Committing(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), ClassTransient},
		// On any other step (login, upload, poll) nothing was posted.
		{"deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), ClassTransient},
		{"net timeout", &net.OpError{Op: "read", Err: timeoutErr{}}, ClassTransient},
		{"reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, ClassTransient},
		{"eof", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), ClassTransient},
		{"504", he(504), ClassTransient},
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

// An HTTPError's message keeps the start of the answer, on one line and
// without credentials or HTML; Body stays whole for the driver.
func TestHTTPErrorMessage(t *testing.T) {
	body := "{\"error\":\"InvalidToken\",\n\"message\":\"token THAA is bad\",\"access_token\":\"THAASECRET\"}" + strings.Repeat(" pad", 200)
	e := &HTTPError{Op: "refresh", StatusCode: 400, Body: body}
	msg := e.Error()
	if !strings.HasPrefix(msg, `refresh returned 400: {"error":"InvalidToken", "message"`) || strings.Contains(msg, "THAASECRET") || len(msg) > 400 {
		t.Errorf("message %q", msg)
	}
	if e.Body != body {
		t.Error("Body changed")
	}
	if got := (&HTTPError{Op: "x", StatusCode: 502, Body: "<html>proxy</html>"}).Error(); got != "x returned 502: (an HTML page)" {
		t.Errorf("html: %q", got)
	}
	if got := (&HTTPError{Op: "x", StatusCode: 500}).Error(); got != "x returned 500" {
		t.Errorf("empty: %q", got)
	}
}

package destination

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestCheckedAuth(t *testing.T) {
	if h := CheckedAuth(nil, false); !h.OK || !h.Reachable || h.AuthOK == nil || !*h.AuthOK {
		t.Errorf("success: %+v", h)
	}
	if h := CheckedAuth(&HTTPError{StatusCode: 401}, true); h.OK || !h.Reachable || h.AuthOK == nil || *h.AuthOK {
		t.Errorf("refused: %+v", h)
	}
	// Only a refusal sets AuthOK: a busy or failing server says nothing
	// about the credentials.
	for _, err := range []error{&HTTPError{StatusCode: 429}, &HTTPError{StatusCode: 502},
		fmt.Errorf("dial: %w", &net.DNSError{Err: "no such host", Name: "x.invalid"})} {
		if h := CheckedAuth(err, false); h.OK || h.AuthOK != nil || h.Message == "" {
			t.Errorf("%v: %+v", err, h)
		}
	}
	if h := CheckedAuth(&HTTPError{StatusCode: 429}, false); !h.Reachable {
		t.Errorf("a rate-limited server is reachable: %+v", h)
	}
	// A network's own rate-limit code on another status (Threads answers
	// 400 or 403 with Graph code 4, 17, 32 or 613) reads as a rate limit.
	graph := &Error{Class: ClassRateLimited, Err: &HTTPError{StatusCode: 400}}
	if h := CheckedAuth(graph, false); !h.Reachable || h.AuthOK != nil || !strings.Contains(h.Message, "rate limiting") {
		t.Errorf("Graph rate limit: %+v", h)
	}
	// A 200 whose body isn't the JSON expected (an HTML page from a proxy)
	// came from a server that answered.
	var v struct{}
	decodeErr := json.Unmarshal([]byte("<html>"), &v)
	if h := CheckedAuth(decodeErr, false); h.OK || !h.Reachable || h.AuthOK != nil || h.Message != "the server's answer could not be read" {
		t.Errorf("unreadable answer: %+v", h)
	}
}

// Messages name the kind of failure, never the request.
func TestNetProblem(t *testing.T) {
	for err, want := range map[error]string{
		context.DeadlineExceeded:                                   "the server did not answer in time",
		&net.DNSError{Err: "no such host", Name: "secret.example"}: "the server's name could not be found",
		errors.New("something else"):                               "the server could not be reached",
	} {
		if got := NetProblem(fmt.Errorf("get https://u:p@secret.example/: %w", err)); got != want {
			t.Errorf("NetProblem(%v) = %q, want %q", err, got, want)
		}
	}
}

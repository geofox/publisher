package threads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// graphServer answers every request with the handler for its step.
func graphServer(t *testing.T, create, poll, publish http.HandlerFunc) *Client {
	t.Helper()
	ok := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	if create == nil {
		create = ok(`{"id":"c1"}`)
	}
	if poll == nil {
		poll = ok(`{"status":"FINISHED","permalink":"https://t/p"}`)
	}
	if publish == nil {
		publish = ok(`{"id":"m1"}`)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/threads_publish"):
			publish(w, r)
		case strings.HasSuffix(r.URL.Path, "/threads"):
			create(w, r)
		default:
			poll(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New("tok", "me")
	c.BaseURL = srv.URL
	c.PollInterval = time.Millisecond
	return c
}

func graphErr(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

// hang blocks until the client gives up.
func hang(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

// The Graph API reports throttling, expired tokens and temporary failures as
// HTTP 400 with the meaning in error.code.
func TestPublishClassifiesGraphErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want destination.ErrorClass
	}{
		{"temporary", `{"error":{"code":2,"is_transient":true}}`, destination.ClassTransient},
		{"unknown retry", `{"error":{"code":1}}`, destination.ClassTransient},
		{"is_transient flag", `{"error":{"code":9007,"is_transient":true}}`, destination.ClassTransient},
		{"app throttled", `{"error":{"code":4}}`, destination.ClassRateLimited},
		{"user throttled", `{"error":{"code":17}}`, destination.ClassRateLimited},
		{"token expired", `{"error":{"code":190,"error_subcode":463}}`, destination.ClassAuth},
		{"permission", `{"error":{"code":200}}`, destination.ClassAuth},
		{"bad param", `{"error":{"code":100}}`, destination.ClassInvalid},
		{"no body", ``, destination.ClassInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := graphServer(t, graphErr(400, tc.body), nil, nil)
			_, err := (&Account{C: c}).Publish(context.Background(), destination.PublishRequest{Text: "hi"})
			if got := destination.ClassOf(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}

// A timeout before threads_publish posted nothing and is safe to retry; a
// timeout on threads_publish itself may have published.
func TestPublishTimeoutClassByStep(t *testing.T) {
	cases := []struct {
		name                  string
		create, poll, publish http.HandlerFunc
		want                  destination.ErrorClass
	}{
		{"container create", hang, nil, nil, destination.ClassTransient},
		{"status poll", nil, hang, nil, destination.ClassTransient},
		{"publish", nil, nil, hang, destination.ClassUnknownOutcome},
		{"publish 504", nil, nil, graphErr(504, ""), destination.ClassUnknownOutcome},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := graphServer(t, tc.create, tc.poll, tc.publish)
			c.HTTP.Timeout = 50 * time.Millisecond
			_, err := (&Account{C: c}).Publish(context.Background(), destination.PublishRequest{Text: "hi"})
			if got := destination.ClassOf(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}

// A context already done when the publish step is reached sent nothing.
func TestPublishCanceledBeforeSendIsTransient(t *testing.T) {
	c := graphServer(t, nil, nil, func(http.ResponseWriter, *http.Request) { t.Error("publish must not be sent") })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.publish(ctx, "c1")
	if got := destination.ClassOf(err); got != destination.ClassTransient {
		t.Fatalf("class = %q, want transient (err %v)", got, err)
	}
}

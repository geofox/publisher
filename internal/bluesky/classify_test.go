package bluesky

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/destination"
)

// A timeout on login posted nothing and is safe to retry; a timeout on
// createRecord may have published.
func TestPublishTimeoutClassByStep(t *testing.T) {
	// hang blocks until the client gives up. The body is drained first: the
	// server only notices a closed connection once it has read the request.
	hang := func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
	}
	session := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me", "accessJwt": "A"})
	}
	cases := []struct {
		name            string
		session, record http.HandlerFunc
		want            destination.ErrorClass
	}{
		{"createSession", hang, nil, destination.ClassTransient},
		{"createRecord", session, hang, destination.ClassUnknownOutcome},
		{"createRecord 504", session, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusGatewayTimeout)
		}, destination.ClassUnknownOutcome},
		{"createRecord 500", session, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}, destination.ClassTransient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/xrpc/com.atproto.server.createSession", tc.session)
			if tc.record != nil {
				mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", tc.record)
			}
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)
			c := New(srv.URL, "me", "pw")
			c.HTTP.Timeout = 50 * time.Millisecond
			_, err := (&Account{C: c}).Publish(context.Background(), destination.PublishRequest{Text: "hi"})
			if got := destination.ClassOf(err); got != tc.want {
				t.Fatalf("class = %q, want %q (err %v)", got, tc.want, err)
			}
		})
	}
}

// A context already done when createRecord is reached sent nothing.
func TestCreateRecordCanceledBeforeSendIsTransient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("createRecord must not be sent")
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := New(srv.URL, "me", "pw").createRecord(ctx, session{Did: "did:plc:abc"}, "app.bsky.feed.post", "", nil)
	if got := destination.ClassOf(err); got != destination.ClassTransient {
		t.Fatalf("class = %q, want transient (err %v)", got, err)
	}
}

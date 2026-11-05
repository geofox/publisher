package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// Server-sent events (spec §6.7): GET /v1/posts/{id}/events for one post,
// GET /v1/events for all the owner's posts. Each event carries its log id;
// a client that reconnects with Last-Event-ID (or ?after=) resumes after
// it. The per-post stream starts at the post's first event and ends once
// the post has nothing left to send; the global one starts now and runs
// until the client leaves.

const (
	sseHeartbeat  = 15 * time.Second
	ssePoll       = time.Second
	sseBatch      = 200
	sseMaxLife    = time.Hour // then the client reconnects with Last-Event-ID
	sseMaxStreams = 64        // open streams per process
)

// v1EventBody is an event's data line.
type v1EventBody struct {
	ID          int64   `json:"id"`
	Type        string  `json:"type"`
	CreatedAt   string  `json:"created_at"`
	Post        string  `json:"post"`
	Status      string  `json:"status,omitempty"`
	Target      string  `json:"target,omitempty"`
	Account     string  `json:"account,omitempty"`
	Destination string  `json:"destination,omitempty"`
	Ordinal     *int    `json:"ordinal,omitempty"`
	URL         string  `json:"url,omitempty"`
	ErrorClass  string  `json:"error_class,omitempty"`
	Error       string  `json:"error,omitempty"`
	Endpoint    string  `json:"endpoint,omitempty"`
	ScheduledAt *string `json:"scheduled_at,omitempty"`
	Hidden      *bool   `json:"hidden,omitempty"`
}

func v1Event(e store.Event) v1EventBody {
	d := e.Data
	b := v1EventBody{
		ID: e.ID, Type: e.Type, CreatedAt: rfc3339(e.CreatedAt), Post: postID(e.PostID), Account: d.AccountID,
		Destination: d.Platform, Ordinal: d.Ordinal, URL: d.RemoteURL, ErrorClass: d.ErrorClass, Error: d.Error,
		Endpoint: d.Endpoint, ScheduledAt: rfc3339ptr(d.ScheduledAt), Hidden: d.Hidden,
	}
	if d.TargetID != 0 {
		b.Target = targetID(d.TargetID)
	}
	switch e.Type {
	case store.EventPostCreated, store.EventPostUpdated, store.EventPostCompleted:
		b.Status = v1Status(d.Status)
	default:
		b.Status = d.Status // item and delivery vocabularies are already /v1's
	}
	return b
}

// lastEventID reads where a client resumes: Last-Event-ID, or ?after=
// (the header wins; both are checked).
func lastEventID(w http.ResponseWriter, r *http.Request) (int64, bool, bool) {
	var n int64
	var given bool
	for _, src := range []struct{ name, value string }{
		{"after", r.URL.Query().Get("after")},
		{"Last-Event-ID", r.Header.Get("Last-Event-ID")},
	} {
		if src.value == "" {
			continue
		}
		v, err := strconv.ParseInt(src.value, 10, 64)
		if err != nil || v < 0 {
			problem(w, r, http.StatusBadRequest, codeInvalidParameter, src.name+" is an event id",
				fieldError{Parameter: src.name, Code: "invalid", Detail: "a whole number from a previous event's id"})
			return 0, false, false
		}
		n, given = v, true
	}
	return n, given, true
}

func (a *API) v1PostEvents(w http.ResponseWriter, r *http.Request) {
	after, _, ok := lastEventID(w, r)
	if !ok {
		return
	}
	p, ok := a.v1OwnPost(w, r)
	if !ok {
		return
	}
	f := store.EventFilter{OwnerID: p.OwnerID, PostID: p.ID, After: after, Limit: 1}
	// Settled with nothing new: 204, which tells an EventSource to stop
	// reconnecting.
	_, settled, err := a.Store.PostState(r.Context(), p.ID)
	if err == nil && settled {
		evs, err := a.Store.EventsSince(r.Context(), f)
		if err == nil && len(evs) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	a.streamEvents(w, r, f, true)
}

func (a *API) v1Events(w http.ResponseWriter, r *http.Request) {
	after, given, ok := lastEventID(w, r)
	if !ok {
		return
	}
	if a.Store == nil {
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
		return
	}
	if !given {
		last, err := a.Store.LastEventID(r.Context())
		if err != nil {
			slog.ErrorContext(r.Context(), "v1: last event id", "err", err)
			problem(w, r, http.StatusInternalServerError, codeInternal, "reading events failed")
			return
		}
		after = last
	}
	pr, _ := principalFrom(r.Context())
	a.streamEvents(w, r, store.EventFilter{OwnerID: pr.OwnerID, After: after}, false)
}

// streamEvents writes events matching f as they are logged. untilSettled
// ends the stream once the post is settled and its events are sent. A
// stream also ends after sseMaxLife or when the server stops; the client
// resumes with Last-Event-ID.
func (a *API) streamEvents(w http.ResponseWriter, r *http.Request, f store.EventFilter, untilSettled bool) {
	if n := a.streams.Add(1); n > sseMaxStreams {
		a.streams.Add(-1)
		w.Header().Set("Retry-After", "30")
		problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "too many event streams are open")
		return
	}
	defer a.streams.Add(-1)
	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return // a HEAD response has no body to stream
	}
	// The server's write timeout is for ordinary requests; a stream pushes
	// it forward on every write.
	extend := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseHeartbeat + 30*time.Second)) }
	extend()
	_, _ = w.Write([]byte("retry: 3000\n\n"))
	_ = rc.Flush()

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()
	poll := time.NewTicker(ssePoll)
	defer poll.Stop()
	life := time.NewTimer(sseMaxLife)
	defer life.Stop()
	ctx := r.Context()
	f.Limit = sseBatch
	lastPost := "" // the type of the last post.* event sent
	for {
		changed := a.Store.EventsChanged() // before reading, so no write slips between
		// The post's state is read before its events: a settled state was
		// committed with its events, so they are all in the read below.
		settled := false
		if untilSettled {
			_, done, err := a.Store.PostState(ctx, f.PostID)
			if err != nil {
				return // gone (an old-style cancel deletes it)
			}
			settled = done
		}
		events, err := a.Store.EventsSince(ctx, f)
		if err != nil {
			if ctx.Err() == nil {
				slog.ErrorContext(ctx, "v1: read events", "err", err)
			}
			return
		}
		for _, e := range events {
			b, _ := json.Marshal(v1Event(e))
			extend()
			if _, err := w.Write([]byte("id: " + strconv.FormatInt(e.ID, 10) + "\nevent: " + e.Type + "\ndata: " + string(b) + "\n\n")); err != nil {
				return
			}
			f.After = e.ID
			if strings.HasPrefix(e.Type, "post.") {
				lastPost = e.Type
			}
		}
		if len(events) > 0 {
			_ = rc.Flush()
		}
		// Settled, everything sent, and the last word was that it settled
		// (not a retry that started after the state was read).
		if settled && len(events) < sseBatch && (lastPost == store.EventPostCompleted || lastPost == "") {
			return
		}
		if len(events) == sseBatch {
			continue // more waiting
		}
		select {
		case <-ctx.Done():
			return
		case <-a.Stopping:
			return
		case <-life.C:
			return
		case <-changed:
		case <-poll.C:
		case <-heartbeat.C:
			extend()
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			_ = rc.Flush()
		}
	}
}

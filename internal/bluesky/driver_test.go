package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// fakePDS answers session and record calls; createRecord for collection
// failOn (if set) answers 500.
func fakePDS(t *testing.T, failOn string, records *[]map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.example.com", "accessJwt": "A", "refreshJwt": "R"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Collection string
			Record     map[string]any
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Collection == failOn {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		*records = append(*records, body.Record)
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/" + body.Collection + "/3kRKEY", "cid": "bafyNEW"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestPublishThreadsWithRefData(t *testing.T) {
	var recs []map[string]any
	a := &Account{C: New(fakePDS(t, "", &recs).URL, "me.example.com", "pw")}
	res, err := a.Publish(context.Background(), destination.PublishRequest{
		Text: "part two",
		InReplyTo: &destination.ThreadRef{
			Root:   destination.ItemRef{ID: "at://root", Data: RefData("bafyROOT")},
			Parent: destination.ItemRef{ID: "at://parent", Data: RefData("bafyPARENT")},
		},
	})
	if err != nil || res.Status != destination.StatusPublished {
		t.Fatalf("publish: %+v %v", res, err)
	}
	if string(res.Ref.Data) != `{"cid":"bafyNEW"}` {
		t.Errorf("ref data = %s, want the new record's CID", res.Ref.Data)
	}
	reply, _ := recs[0]["reply"].(map[string]any)
	root, _ := reply["root"].(map[string]any)
	parent, _ := reply["parent"].(map[string]any)
	if root["cid"] != "bafyROOT" || parent["uri"] != "at://parent" || parent["cid"] != "bafyPARENT" {
		t.Fatalf("reply refs = %#v", reply)
	}
}

func TestPublishExternalReplyUsesThreadRoot(t *testing.T) {
	var recs []map[string]any
	a := &Account{C: New(fakePDS(t, "", &recs).URL, "me.example.com", "pw")}
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: "hi",
		ReplyTo: &destination.SourceRef{URI: "at://src", CID: "c1", ReplyRootURI: "at://top", ReplyRootCID: "c0"}})
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := recs[0]["reply"].(map[string]any)
	root, _ := reply["root"].(map[string]any)
	if root["uri"] != "at://top" {
		t.Fatalf("a reply to a reply must keep the thread root: %#v", reply)
	}
}

// The post is live even when its reply gate fails to write: partial, with the
// link kept.
func TestPublishGateFailureIsPartial(t *testing.T) {
	var recs []map[string]any
	a := &Account{C: New(fakePDS(t, "app.bsky.feed.threadgate", &recs).URL, "me.example.com", "pw")}
	res, err := a.Publish(context.Background(), destination.PublishRequest{Text: "gated",
		Options: map[string]any{OptReplyGate: "nobody"}})
	if err == nil || res.Status != destination.StatusPartial || res.Ref.ID == "" {
		t.Fatalf("want partial with the post's ref: %+v %v", res, err)
	}
}

func TestPublishTooLongIsInvalid(t *testing.T) {
	var recs []map[string]any
	a := &Account{C: New(fakePDS(t, "", &recs).URL, "me.example.com", "pw")}
	_, err := a.Publish(context.Background(), destination.PublishRequest{Text: strings.Repeat("a", maxGraphemes+1)})
	if destination.ClassOf(err) != destination.ClassInvalid || len(recs) != 0 {
		t.Fatalf("an over-long post must fail as invalid before any write: %v (%d records)", err, len(recs))
	}
}

func TestPublishAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"AuthenticationRequired"}`))
	}))
	defer srv.Close()
	_, err := (&Account{C: New(srv.URL, "me", "bad")}).Publish(context.Background(), destination.PublishRequest{Text: "x"})
	if destination.ClassOf(err) != destination.ClassAuth {
		t.Fatalf("a rejected login must be an auth failure: %v", err)
	}
}

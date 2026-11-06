package bluesky

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func qaLinks(text string) []string {
	var out []string
	for _, f := range parseFacets(text, nil) {
		if u := f.linkURI(); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func qaTags(text string) []string {
	var out []string
	for _, f := range parseFacets(text, nil) {
		for _, ft := range f.Features {
			if ft.Type == "app.bsky.richtext.facet#tag" {
				out = append(out, ft.Tag)
			}
		}
	}
	return out
}

// A URL whose own path carries balanced parentheses keeps its closing ')'.
func TestFacetKeepsBalancedParen(t *testing.T) {
	const u = "https://en.wikipedia.org/wiki/Go_(programming_language)"
	got := qaLinks("read " + u + " now")
	if len(got) != 1 || got[0] != u {
		t.Errorf("link facet = %q, want %q", got, u)
	}
}

// Closing quotes and brackets after a URL are not part of it.
func TestFacetDropsTrailingQuote(t *testing.T) {
	got := qaLinks(`he said "https://example.com/a" and left`)
	if len(got) != 1 || got[0] != "https://example.com/a" {
		t.Errorf("link facet = %q, want https://example.com/a", got)
	}
}

// Bluesky only treats '#' as a tag after whitespace/start, and never a tag
// that starts with a digit.
func TestTagNotMidWordOrNumeric(t *testing.T) {
	if got := qaTags("see issue#123 and C#sharp or #1 fan"); len(got) != 0 {
		t.Errorf("tags = %q, want none", got)
	}
}

// Combining marks are part of a word: a Hindi tag must not be cut.
func TestTagKeepsCombiningMarks(t *testing.T) {
	const tag = "हिन्दी"
	if got := qaTags("#" + tag); len(got) != 1 || got[0] != tag {
		t.Errorf("tags = %q, want [%q]", got, tag)
	}
}

// With reply gate "mention", only actors mentioned in the post (mention
// facets) can reply, so Post resolves @handles and writes mention facets.
// A handle that doesn't resolve stays plain text.
func TestMentionGateHasMentionFacet(t *testing.T) {
	var record map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:me", "handle": "me.test", "accessJwt": "A", "refreshJwt": "R"})
	})
	mux.HandleFunc("/xrpc/com.atproto.identity.resolveHandle", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("handle") != "bob.bsky.social" {
			http.Error(w, `{"error":"InvalidRequest"}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:bob"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Collection string         `json:"collection"`
			Record     map[string]any `json:"record"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Collection == "app.bsky.feed.post" {
			record = body.Record
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:me/app.bsky.feed.post/3k", "cid": "bafy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	text := "hey @Bob.bsky.social and @nobody.example what do you think"
	if _, err := New(srv.URL, "me.test", "pw").Post(context.Background(), Post{Text: text, ReplyGate: ParseReplyGate("mention")}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record["facets"])
	var fs []facet
	_ = json.Unmarshal(raw, &fs)
	var got []string
	for _, f := range fs {
		for _, ft := range f.Features {
			if ft.Type == "app.bsky.richtext.facet#mention" {
				got = append(got, text[f.Index.ByteStart:f.Index.ByteEnd]+"="+ft.DID)
			}
		}
	}
	if len(got) != 1 || got[0] != "@Bob.bsky.social=did:plc:bob" {
		t.Errorf("mention facets = %q, want [@Bob.bsky.social=did:plc:bob]", got)
	}
}

// Every Post signs in again (createSession is limited to 30/5 min and
// 300/day per account).
func TestPostReusesSession(t *testing.T) {
	var sessions atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/xrpc/com.atproto.server.createSession", func(w http.ResponseWriter, r *http.Request) {
		sessions.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"did": "did:plc:abc", "handle": "me.test", "accessJwt": "A", "refreshJwt": "R"})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.createRecord", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"uri": "at://did:plc:abc/app.bsky.feed.post/3k", "cid": "bafy"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(srv.URL, "me.test", "pw")
	for i := 0; i < 5; i++ {
		if _, err := c.Post(context.Background(), Post{Text: "segment"}); err != nil {
			t.Fatal(err)
		}
	}
	if n := sessions.Load(); n > 1 {
		t.Errorf("5 posts made %d createSession calls; a 31-post thread hits the 30/5min limit", n)
	}
}

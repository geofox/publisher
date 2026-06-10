package unfurl

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newAtprotoTestServer fakes both the PLC directory and the owner's PDS on one
// mux: /did:plc:abc returns a DID document whose PDS endpoint is the server
// itself; getRecord returns the canonical uri + cid.
func newAtprotoTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/did:plc:abc", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "did:plc:abc",
			"service": []map[string]any{{
				"id": "#atproto_pds", "type": "AtprotoPersonalDataServer",
				"serviceEndpoint": srv.URL,
			}},
		})
	})
	mux.HandleFunc("/xrpc/com.atproto.repo.getRecord", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("repo") != "did:plc:abc" ||
			r.URL.Query().Get("collection") != "site.standard.document" ||
			r.URL.Query().Get("rkey") != "3k" {
			http.Error(w, "bad query", 400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"uri": "at://did:plc:abc/site.standard.document/3k",
			"cid": "bafyreigh2akiscaild",
		})
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveRef(t *testing.T) {
	srv := newAtprotoTestServer(t)
	s := &Service{HTTP: srv.Client(), PLCDirectory: srv.URL}
	ref, err := s.resolveRef(context.Background(), "at://did:plc:abc/site.standard.document/3k")
	if err != nil {
		t.Fatal(err)
	}
	if ref.URI != "at://did:plc:abc/site.standard.document/3k" || ref.CID != "bafyreigh2akiscaild" {
		t.Fatalf("ref: %+v", ref)
	}
}

func TestResolveRefRejectsNonDID(t *testing.T) {
	s := &Service{HTTP: http.DefaultClient}
	if _, err := s.resolveRef(context.Background(), "at://alice.example.com/site.standard.document/3k"); err == nil {
		t.Fatal("expected error for handle-based at:// uri")
	}
	if _, err := s.resolveRef(context.Background(), "https://not-at.example.com/x"); err == nil {
		t.Fatal("expected error for non-at:// uri")
	}
}

func TestDIDWebDocURL(t *testing.T) {
	cases := []struct{ did, want string }{
		{"did:web:example.com", "https://example.com/.well-known/did.json"},
		{"did:web:example.com:users:alice", "https://example.com/users/alice/did.json"},
	}
	for _, c := range cases {
		if got := didWebDocURL(c.did); got != c.want {
			t.Fatalf("didWebDocURL(%q) = %q, want %q", c.did, got, c.want)
		}
	}
}

func TestResolveRefNoPDSService(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/did:plc:nopds", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "did:plc:nopds",
			"service": []map[string]any{
				{"id": "#other", "type": "SomethingElse", "serviceEndpoint": "https://x.example"},
				{"id": "#atproto_pds", "type": "AtprotoPersonalDataServer", "serviceEndpoint": ""},
			},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &Service{HTTP: srv.Client(), PLCDirectory: srv.URL}
	_, err := s.resolveRef(context.Background(), "at://did:plc:nopds/site.standard.document/3k")
	if err == nil || !strings.Contains(err.Error(), "no atproto_pds service") {
		t.Fatalf("expected no-pds-service error, got %v", err)
	}
}

func TestGetJSONErrorIncludesBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"RecordNotFound"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s := &Service{HTTP: srv.Client()}
	err := s.getJSON(context.Background(), srv.URL+"/x", &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "RecordNotFound") {
		t.Fatalf("error must include response body, got %v", err)
	}
}

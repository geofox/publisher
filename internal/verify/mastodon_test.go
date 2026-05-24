package verify

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mr-tron/base58"
)

// apFixtureServer serves an AP object at /note and an actor at /users/alice, both
// on the same host (so origin authority holds). Returns the note URL.
//
// When withProof is false it serves an unsigned note and a minimal actor (origin
// assurance only). When withProof is true it serves a note carrying a valid
// FEP-8b32 eddsa-jcs-2022 integrity proof and an actor whose assertionMethod
// holds the matching Multikey, so the production verifier accepts it.
func apFixtureServer(t *testing.T, withProof bool) (*httptest.Server, string) {
	t.Helper()
	if withProof {
		return signedFixtureServer(t, func(content string) string { return content }, "assertionMethod")
	}
	mux := http.NewServeMux()
	var base string
	mux.HandleFunc("/users/alice", func(w http.ResponseWriter, r *http.Request) {
		actor := map[string]any{
			"id":                base + "/users/alice",
			"type":              "Person",
			"preferredUsername": "alice",
		}
		writeJSONLD(w, actor)
	})
	mux.HandleFunc("/note", func(w http.ResponseWriter, r *http.Request) {
		note := map[string]any{
			"id":           base + "/note",
			"type":         "Note",
			"attributedTo": base + "/users/alice",
			"content":      "hello fediverse",
			"published":    "2026-05-01T00:00:00Z",
		}
		writeJSONLD(w, note)
	})
	srv := httptest.NewServer(mux)
	base = srv.URL
	return srv, srv.URL + "/note"
}

// signedFixtureServer serves a note signed with a freshly generated ed25519 key
// and an actor carrying the matching assertionMethod Multikey. The signature is
// always computed over a note whose content is "signed hello" using the
// package's own canonicalize, so a faithful verifier accepts it. proofPurpose
// sets the proof's purpose (use "assertionMethod" for a valid proof). tamper is
// applied to the served note's content AFTER signing: pass the identity function
// for an untampered fixture, or a mutation to make the served document diverge
// from the signed bytes (simulating wire tampering). Returns the note URL.
func signedFixtureServer(t *testing.T, tamper func(string) string, proofPurpose string) (*httptest.Server, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	multikey := "z" + base58.Encode(append([]byte{0xed, 0x01}, pub...))

	const signedContent = "signed hello"

	var (
		once     sync.Once
		base     string
		vmID     string
		noteJSON []byte
	)

	build := func() {
		vmID = base + "/users/alice#ed25519-key"
		note := map[string]any{
			"id":           base + "/note",
			"type":         "Note",
			"attributedTo": base + "/users/alice",
			"content":      signedContent,
			"published":    "2026-05-01T00:00:00Z",
		}
		proofOpts := map[string]any{
			"type":               "DataIntegrityProof",
			"cryptosuite":        "eddsa-jcs-2022",
			"verificationMethod": vmID,
			"proofPurpose":       proofPurpose,
			"created":            "2026-05-01T00:00:00Z",
		}
		docCanon, err := canonicalize(note)
		if err != nil {
			t.Fatalf("canonicalize note: %v", err)
		}
		proofCanon, err := canonicalize(proofOpts)
		if err != nil {
			t.Fatalf("canonicalize proofOptions: %v", err)
		}
		dh := sha256.Sum256(docCanon)
		ph := sha256.Sum256(proofCanon)
		sig := ed25519.Sign(priv, append(ph[:], dh[:]...))

		// Full proof object = proofOptions + proofValue.
		proof := map[string]any{}
		for k, val := range proofOpts {
			proof[k] = val
		}
		proof["proofValue"] = "z" + base58.Encode(sig)

		// Apply tampering to the served content only (signature stays over the
		// original signedContent), then attach the proof.
		note["content"] = tamper(signedContent)
		note["proof"] = proof

		noteJSON, err = json.Marshal(note)
		if err != nil {
			t.Fatalf("marshal note: %v", err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/users/alice", func(w http.ResponseWriter, r *http.Request) {
		once.Do(build)
		actor := map[string]any{
			"id":                base + "/users/alice",
			"type":              "Person",
			"preferredUsername": "alice",
			"assertionMethod": []map[string]any{{
				"id":                 vmID,
				"type":               "Multikey",
				"controller":         base + "/users/alice",
				"publicKeyMultibase": multikey,
			}},
		}
		writeJSONLD(w, actor)
	})
	mux.HandleFunc("/note", func(w http.ResponseWriter, r *http.Request) {
		once.Do(build)
		w.Header().Set("Content-Type", "application/activity+json")
		_, _ = w.Write(noteJSON)
	})

	srv := httptest.NewServer(mux)
	base = srv.URL
	return srv, srv.URL + "/note"
}

func writeJSONLD(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(v)
}

func mastodonVerifier() *MastodonVerifier {
	mv := NewMastodonVerifier(time.Second)
	// Tests serve on 127.0.0.1; swap in a plain client so the SSRF guard (which
	// blocks loopback) doesn't reject the test server. Production uses the guarded client.
	mv.http = &http.Client{Timeout: time.Second}
	return mv
}

func TestThreadsOriginVerified(t *testing.T) {
	// Threads uses the same ActivityPub verifier as Mastodon, only labeled
	// "threads". It can only ever reach origin assurance (Threads emits no
	// FEP-8b32 proofs), which this fixture (no proof) exercises.
	srv, noteURL := apFixtureServer(t, false)
	defer srv.Close()
	tv := NewThreadsVerifier(time.Second)
	tv.http = &http.Client{Timeout: time.Second} // plain client for loopback fixture
	v := tv.Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s err=%s checks=%+v", v.Status, v.Error, v.Checks)
	}
	if v.Platform != "threads" {
		t.Errorf("platform = %q, want threads", v.Platform)
	}
	if v.Assurance != "origin" {
		t.Errorf("assurance = %q, want origin", v.Assurance)
	}
	if !hasCheck(v, "origin_authority", "pass") {
		t.Errorf("missing origin_authority pass: %+v", v.Checks)
	}
}

// htmlServer serves an HTML page (ignoring AP content negotiation, like Threads
// web URLs and many permalinks).
func htmlServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>not activitypub</body></html>"))
	}))
}

func TestMastodonHTMLPageIsError(t *testing.T) {
	srv := htmlServer(t)
	defer srv.Close()
	mv := mastodonVerifier()
	v := mv.Verify(context.Background(), Input{Raw: srv.URL + "/@x/post/y"})
	if v.Status != StatusError {
		t.Fatalf("status = %s, want error", v.Status)
	}
	if !strings.Contains(v.Error, "HTML page") {
		t.Errorf("expected generic HTML error, got: %q", v.Error)
	}
}

func TestThreadsHTMLPageHasThreadsMessage(t *testing.T) {
	// Threads gates AP behind authenticated federation; the error should say so,
	// not suggest pasting an AP object URL (which is also gated for Threads).
	srv := htmlServer(t)
	defer srv.Close()
	tv := NewThreadsVerifier(time.Second)
	tv.http = &http.Client{Timeout: time.Second}
	v := tv.Verify(context.Background(), Input{Raw: srv.URL + "/@x/post/y"})
	if v.Status != StatusError {
		t.Fatalf("status = %s, want error", v.Status)
	}
	if !strings.Contains(v.Error, "Threads") || strings.Contains(v.Error, "paste the ActivityPub") {
		t.Errorf("expected Threads-specific message, got: %q", v.Error)
	}
}

func TestMastodonOriginVerified(t *testing.T) {
	srv, noteURL := apFixtureServer(t, false)
	defer srv.Close()
	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s err=%s checks=%+v", v.Status, v.Error, v.Checks)
	}
	if v.Assurance != "origin" {
		t.Errorf("assurance = %q, want origin", v.Assurance)
	}
	if !hasCheck(v, "origin_authority", "pass") {
		t.Errorf("missing origin_authority pass: %+v", v.Checks)
	}
	if v.Signer == nil || v.Signer.Acct != "alice@"+strings.TrimPrefix(srv.URL, "http://") {
		t.Errorf("acct = %+v (want alice@host)", v.Signer)
	}
}

func TestMastodonOriginMismatchFails(t *testing.T) {
	// Two servers: noteSrv serves a Note whose attributedTo points at actorSrv
	// (a DIFFERENT host). Both fetches succeed, but the object host and actor
	// host differ → origin authority fails (StatusFailed, not StatusError).
	var actorURL string
	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONLD(w, map[string]any{"id": actorURL, "type": "Person", "preferredUsername": "alice"})
	}))
	defer actorSrv.Close()
	actorURL = actorSrv.URL + "/users/alice"

	var noteURL string
	noteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSONLD(w, map[string]any{"id": noteURL, "type": "Note",
			"attributedTo": actorURL, "content": "x"})
	}))
	defer noteSrv.Close()
	noteURL = noteSrv.URL + "/note"

	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusFailed {
		t.Fatalf("status = %s (err=%s), want failed (origin mismatch)", v.Status, v.Error)
	}
	if !hasCheck(v, "origin_authority", "fail") {
		t.Errorf("expected origin_authority=fail, got %+v", v.Checks)
	}
}

func TestMastodonExpectedAcctMatch(t *testing.T) {
	srv, noteURL := apFixtureServer(t, false)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL, Expected: "@alice@" + host})
	if v.Expected == nil || !v.Expected.Matches {
		t.Fatalf("expected acct match, got %+v", v.Expected)
	}
}

func TestMastodonFEP8b32Valid(t *testing.T) {
	srv, noteURL := apFixtureServer(t, true)
	defer srv.Close()
	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s err=%s checks=%+v", v.Status, v.Error, v.Checks)
	}
	if v.Assurance != "cryptographic" {
		t.Errorf("assurance = %q, want cryptographic", v.Assurance)
	}
	if !hasCheck(v, "integrity_proof", "pass") {
		t.Errorf("missing integrity_proof pass: %+v", v.Checks)
	}
}

func TestMastodonFEP8b32Tampered(t *testing.T) {
	// Serve a signed object, then tamper the content on the wire so the proof
	// no longer matches → StatusFailed with integrity_proof fail. The signature
	// is computed over the original content; the served document carries mutated
	// content, so canonicalize(document) no longer matches the signed hash.
	srv, noteURL := signedFixtureServer(t, func(content string) string {
		return content + " TAMPERED"
	}, "assertionMethod")
	defer srv.Close()
	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusFailed {
		t.Fatalf("status = %s err=%s checks=%+v, want failed", v.Status, v.Error, v.Checks)
	}
	if !hasCheck(v, "integrity_proof", "fail") {
		t.Errorf("missing integrity_proof fail: %+v", v.Checks)
	}
}

func TestMastodonFEP8b32WrongPurposeFails(t *testing.T) {
	// Sign a cryptographically valid proof (signature is over proofOptions that
	// include proofPurpose="authentication"), so the only thing wrong is the
	// purpose. The verifier must still reject it, proving proofPurpose is
	// enforced independently of signature validity.
	srv, noteURL := signedFixtureServer(t, func(content string) string { return content }, "authentication")
	defer srv.Close()
	v := mastodonVerifier().Verify(context.Background(), Input{Raw: noteURL})
	if v.Status != StatusFailed {
		t.Fatalf("status = %s err=%s checks=%+v, want failed", v.Status, v.Error, v.Checks)
	}
	if !hasCheck(v, "integrity_proof", "fail") {
		t.Errorf("missing integrity_proof fail: %+v", v.Checks)
	}
}

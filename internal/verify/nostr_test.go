package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const goodEvent = `{"kind":1,"id":"dc90c95f09947507c1044e8f48bcf6350aa6bff1507dd4acfc755b9239b5c962","pubkey":"3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d","created_at":1644271588,"tags":[],"content":"now that https://blueskyweb.org/blog/2-7-2022-overview was announced we can stop working on nostr?","sig":"230e9d8f0ddaf7eb70b5f7741ccfa37e87a455c9a469282e3464e2052d3192cd63a167e196e381ef9d7e69e9ea43af2443b839974dc85d8aaab9efe1d9296524"}`

func nostrVerifier() *NostrVerifier { return &NostrVerifier{HTTP: newSafeClient(0)} }

func TestNostrVerified(t *testing.T) {
	v := nostrVerifier().Verify(context.Background(), Input{Raw: goodEvent})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s, checks=%+v err=%s", v.Status, v.Checks, v.Error)
	}
	if v.Assurance != "cryptographic" {
		t.Errorf("assurance = %q", v.Assurance)
	}
	if v.Signer == nil || v.Signer.PubkeyHex != "3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d" {
		t.Errorf("signer = %+v", v.Signer)
	}
	if v.Signer.Npub == "" || !strings.HasPrefix(v.Signer.Npub, "npub1") {
		t.Errorf("npub not encoded: %+v", v.Signer)
	}
}

func TestNostrTamperedContentFails(t *testing.T) {
	bad := strings.Replace(goodEvent, "we can stop working on nostr?", "we can stop working on nostr!", 1)
	v := nostrVerifier().Verify(context.Background(), Input{Raw: bad})
	if v.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", v.Status)
	}
	if !hasCheck(v, "id_matches", "fail") {
		t.Errorf("expected id_matches=fail, got %+v", v.Checks)
	}
}

func TestNostrTamperedSigFails(t *testing.T) {
	// Flip the first hex digit of the signature (still 64-byte hex, wrong value).
	bad := strings.Replace(goodEvent, `"sig":"230e9d8f`, `"sig":"330e9d8f`, 1)
	v := nostrVerifier().Verify(context.Background(), Input{Raw: bad})
	if v.Status != StatusFailed {
		t.Fatalf("status = %s, want failed", v.Status)
	}
	if !hasCheck(v, "signature_valid", "fail") {
		t.Errorf("expected signature_valid=fail, got %+v", v.Checks)
	}
}

func TestNostrMalformedIsError(t *testing.T) {
	v := nostrVerifier().Verify(context.Background(), Input{Raw: "not json"})
	if v.Status != StatusError {
		t.Fatalf("status = %s, want error", v.Status)
	}
}

func TestNostrExpectedNpubMatch(t *testing.T) {
	// npub of 3bf0c63f...459d
	npub := "npub180cvv07tjdrrgpa0j7j7tmnyl2yr6yr7l8j4s3evf6u64th6gkwsyjh6w6"
	v := nostrVerifier().Verify(context.Background(), Input{Raw: goodEvent, Expected: npub})
	if v.Expected == nil || !v.Expected.Matches {
		t.Fatalf("expected match, got %+v", v.Expected)
	}
}

func TestNostrExpectedHexMismatch(t *testing.T) {
	other := "0000000000000000000000000000000000000000000000000000000000000001"
	v := nostrVerifier().Verify(context.Background(), Input{Raw: goodEvent, Expected: other})
	if v.Expected == nil || v.Expected.Matches {
		t.Fatalf("expected mismatch, got %+v", v.Expected)
	}
}

func TestNostrExpectedNIP05Match(t *testing.T) {
	// well-known nostr.json mapping "alice" -> the goodEvent pubkey.
	// IdentifierToURL always produces https://, so we use NewTLSServer.
	// ParseIdentifier's regex rejects host:port forms, so we use a custom
	// RoundTripper that rewrites the request URL host to the TLS test server.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"names":{"alice":"3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d"}}`))
	}))
	defer srv.Close()
	srvHost := strings.TrimPrefix(srv.URL, "https://") // 127.0.0.1:PORT

	// Wrap srv.Client()'s transport to redirect any host to the test server.
	base := srv.Client().Transport
	nv := &NostrVerifier{HTTP: &http.Client{
		Transport: redirectTransport{base: base, target: srvHost},
	}}
	// alice@127.0.0.1 passes ParseIdentifier's regex (no port needed).
	// The redirectTransport sends the request to the actual test server.
	v := nv.Verify(context.Background(), Input{Raw: goodEvent, Expected: "alice@127.0.0.1"})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s", v.Status)
	}
	if v.Expected == nil || !v.Expected.Matches {
		t.Fatalf("expected NIP-05 match, got %+v", v.Expected)
	}
}

// redirectTransport rewrites any outgoing request's host to target,
// then delegates to base. Used to redirect NIP-05 test lookups to an
// httptest.TLSServer without touching DNS.
type redirectTransport struct {
	base   http.RoundTripper
	target string // host:port
}

func (rt redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r2 := req.Clone(req.Context())
	r2.URL.Host = rt.target
	return rt.base.RoundTrip(r2)
}

// hasCheck reports whether v contains a check with the given name and result.
func hasCheck(v Verdict, name, result string) bool {
	for _, c := range v.Checks {
		if c.Name == name && c.Result == result {
			return true
		}
	}
	return false
}

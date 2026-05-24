package verify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// Fixtures captured from the real bsky.app repo. The CAR is content-addressed
// and immutable, so once captured it is a permanent valid cryptographic proof
// regardless of later repo mutations or deletions.
//
//	DID:        did:plc:z72i7hdynmk6r22z27h6tvur (bsky.app)
//	collection: app.bsky.feed.post
//	rkey:       3mlvllqtnmk2g
const (
	fixtureDID        = "did:plc:z72i7hdynmk6r22z27h6tvur"
	fixtureCollection = "app.bsky.feed.post"
	fixtureRkey       = "3mlvllqtnmk2g"
)

// fakeDir is an identity.Directory that always returns the injected identity,
// letting tests bypass network DID resolution.
type fakeDir struct{ ident *identity.Identity }

func (f *fakeDir) Lookup(_ context.Context, _ syntax.AtIdentifier) (*identity.Identity, error) {
	return f.ident, nil
}

func (f *fakeDir) LookupHandle(_ context.Context, _ syntax.Handle) (*identity.Identity, error) {
	return f.ident, nil
}

func (f *fakeDir) LookupDID(_ context.Context, _ syntax.DID) (*identity.Identity, error) {
	return f.ident, nil
}

func (f *fakeDir) Purge(_ context.Context, _ syntax.AtIdentifier) error { return nil }

// loadFixtureIdentity builds an *identity.Identity from the captured DID
// document, overriding the PDS service endpoint to point at the loopback
// fixture server. If wrongKey is true, the atproto signing key is replaced with
// a freshly generated (valid-format, but unrelated) key so that PublicKey()
// still parses but VerifySignature must fail.
func loadFixtureIdentity(t *testing.T, srvURL string, wrongKey bool) *identity.Identity {
	t.Helper()

	docBytes, err := os.ReadFile("testdata/bsky_diddoc.json")
	if err != nil {
		t.Fatalf("read DID doc fixture: %v", err)
	}
	var doc identity.DIDDocument
	if err := json.Unmarshal(docBytes, &doc); err != nil {
		t.Fatalf("unmarshal DID doc: %v", err)
	}

	id := identity.ParseIdentity(&doc)

	// Point the PDS endpoint at the loopback fixture server. PDSEndpoint()
	// reads Services["atproto_pds"].
	id.Services["atproto_pds"] = identity.ServiceEndpoint{
		Type: "AtprotoPersonalDataServer",
		URL:  srvURL,
	}
	if got := id.PDSEndpoint(); got != srvURL {
		t.Fatalf("PDSEndpoint override failed: got %q want %q", got, srvURL)
	}

	if wrongKey {
		// Generate an unrelated key whose multibase still parses (so
		// PublicKey() succeeds) but which never signed the captured commit.
		priv, err := atcrypto.GeneratePrivateKeyP256()
		if err != nil {
			t.Fatalf("generate wrong key: %v", err)
		}
		pub, err := priv.PublicKey()
		if err != nil {
			t.Fatalf("derive wrong public key: %v", err)
		}
		// PublicKey() reads Keys["atproto"]. Replace it with the wrong key.
		id.Keys["atproto"] = identity.VerificationMethod{
			Type:               "Multikey",
			PublicKeyMultibase: pub.Multibase(),
		}
		if _, err := id.PublicKey(); err != nil {
			t.Fatalf("wrong key must still parse via PublicKey(): %v", err)
		}
	}

	return &id
}

// newCARFixtureServer serves the captured CAR bytes for any getRecord request.
func newCARFixtureServer(t *testing.T) *httptest.Server {
	t.Helper()
	carBytes, err := os.ReadFile("testdata/bsky_post.car")
	if err != nil {
		t.Fatalf("read CAR fixture: %v", err)
	}
	if len(carBytes) == 0 {
		t.Fatal("CAR fixture is empty")
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xrpc/com.atproto.sync.getRecord" {
			t.Errorf("unexpected fixture request path: %s", r.URL.Path)
		}
		_, _ = w.Write(carBytes)
	}))
}

// TestBlueskyOfflineVerified proves that the real captured CAR, verified against
// the real atproto key from the DID document, produces StatusVerified with both
// the commit_signature and mst_inclusion checks passing.
func TestBlueskyOfflineVerified(t *testing.T) {
	srv := newCARFixtureServer(t)
	defer srv.Close()

	ident := loadFixtureIdentity(t, srv.URL, false)
	bv := newBlueskyVerifierWithDir(&fakeDir{ident}, srv.Client())

	v := bv.Verify(context.Background(), Input{
		Raw: "at://" + fixtureDID + "/" + fixtureCollection + "/" + fixtureRkey,
	})

	if v.Status != StatusVerified {
		t.Fatalf("status = %s (err=%q) checks=%+v", v.Status, v.Error, v.Checks)
	}
	if !hasCheck(v, "commit_signature", "pass") {
		t.Errorf("expected commit_signature pass: %+v", v.Checks)
	}
	if !hasCheck(v, "mst_inclusion", "pass") {
		t.Errorf("expected mst_inclusion pass: %+v", v.Checks)
	}
	if v.Signer == nil || v.Signer.DID != fixtureDID {
		t.Errorf("signer not populated correctly: %+v", v.Signer)
	}
}

// TestBlueskyOfflineWrongKeyFails proves the core security property: when the
// DID directory hands back a different (valid-format) atproto key, the captured
// commit's signature must NOT verify, yielding StatusFailed with a
// commit_signature fail (not StatusError — the check completed, the signature is
// simply invalid).
func TestBlueskyOfflineWrongKeyFails(t *testing.T) {
	srv := newCARFixtureServer(t)
	defer srv.Close()

	ident := loadFixtureIdentity(t, srv.URL, true)
	bv := newBlueskyVerifierWithDir(&fakeDir{ident}, srv.Client())

	v := bv.Verify(context.Background(), Input{
		Raw: "at://" + fixtureDID + "/" + fixtureCollection + "/" + fixtureRkey,
	})

	if v.Status != StatusFailed {
		t.Fatalf("status = %s (err=%q) checks=%+v; want failed", v.Status, v.Error, v.Checks)
	}
	if !hasCheck(v, "commit_signature", "fail") {
		t.Errorf("expected commit_signature fail: %+v", v.Checks)
	}
}

// TestBlueskyOfflineMissingRecordFails proves that requesting a record absent
// from the (validly signed) commit's Merkle tree yields StatusFailed with an
// mst_inclusion fail. The signature still verifies (real key, real CAR), but the
// bogus rkey is not in the tree.
func TestBlueskyOfflineMissingRecordFails(t *testing.T) {
	srv := newCARFixtureServer(t)
	defer srv.Close()

	ident := loadFixtureIdentity(t, srv.URL, false)
	bv := newBlueskyVerifierWithDir(&fakeDir{ident}, srv.Client())

	v := bv.Verify(context.Background(), Input{
		Raw: "at://" + fixtureDID + "/" + fixtureCollection + "/doesnotexist999",
	})

	if v.Status != StatusFailed {
		t.Fatalf("status = %s (err=%q) checks=%+v; want failed", v.Status, v.Error, v.Checks)
	}
	if !hasCheck(v, "commit_signature", "pass") {
		t.Errorf("expected commit_signature pass (real key/CAR): %+v", v.Checks)
	}
	if !hasCheck(v, "mst_inclusion", "fail") {
		t.Errorf("expected mst_inclusion fail: %+v", v.Checks)
	}
}

package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/repo"
	"github.com/bluesky-social/indigo/atproto/syntax"
	cbornode "github.com/ipfs/go-ipld-cbor"
)

// BlueskyVerifier verifies an ATProto record by fetching the signed commit + MST
// proof from the author's PDS and checking both the commit signature (against the
// DID's atproto key) and the record's inclusion in the signed tree.
type BlueskyVerifier struct {
	dir  identity.Directory
	http *http.Client
}

// NewBlueskyVerifier builds a verifier whose DID directory and PDS fetches both
// use the SSRF-guarded client. plcURL is the did:plc resolver base URL.
func NewBlueskyVerifier(plcURL string, timeout time.Duration) *BlueskyVerifier {
	client := NewSafeClient(timeout)
	dir := &identity.BaseDirectory{
		PLCURL:     strings.TrimRight(plcURL, "/"),
		HTTPClient: *client,
	}
	return &BlueskyVerifier{dir: dir, http: client}
}

// newBlueskyVerifierWithDir is used by tests to inject a fake directory and a
// plain (non-SSRF-guarded) HTTP client so a loopback fixture server is reachable.
func newBlueskyVerifierWithDir(dir identity.Directory, client *http.Client) *BlueskyVerifier {
	return &BlueskyVerifier{dir: dir, http: client}
}

func (bv *BlueskyVerifier) Verify(ctx context.Context, in Input) Verdict {
	v := Verdict{Platform: "bluesky", Assurance: "cryptographic", Checks: []Check{}}

	authority, collection, rkey, err := parseBlueskyRef(in.Raw)
	if err != nil {
		return errVerdict("bluesky", err.Error())
	}

	atid, err := syntax.ParseAtIdentifier(authority) // returns a value, not a pointer
	if err != nil {
		return errVerdict("bluesky", "invalid author identifier: "+err.Error())
	}
	ident, err := bv.dir.Lookup(ctx, atid)
	if err != nil {
		return errVerdict("bluesky", "could not resolve identity: "+err.Error())
	}

	handleVerified := ident.Handle != syntax.HandleInvalid
	v.Checks = append(v.Checks, check("handle_binding", handleVerified,
		"handle and DID verify bidirectionally"))
	hv := handleVerified
	v.Signer = &Signer{
		DID: ident.DID.String(), Handle: ident.Handle.String(),
		HandleVerified: &hv, PDS: ident.PDSEndpoint(),
	}

	pds := ident.PDSEndpoint()
	if pds == "" {
		return errVerdict("bluesky", "no PDS endpoint in DID document")
	}

	car, err := bv.fetchCAR(ctx, pds, ident.DID.String(), collection, rkey)
	if err != nil {
		return errVerdict("bluesky", "could not fetch record: "+err.Error())
	}

	commit, r, err := repo.LoadRepoFromCAR(ctx, bytes.NewReader(car))
	if err != nil {
		return errVerdict("bluesky", "could not parse repo CAR: "+err.Error())
	}

	// The CAR's commit must be for the repo we resolved, else the proof is moot.
	if !strings.EqualFold(commit.DID, ident.DID.String()) {
		v.Checks = append(v.Checks, Check{Name: "commit_signature", Result: "fail",
			Detail: "commit DID does not match resolved identity"})
		v.Status = StatusFailed
		return v
	}

	if err := commit.VerifyStructure(); err != nil {
		v.Checks = append(v.Checks, Check{Name: "commit_signature", Result: "fail",
			Detail: "invalid commit structure: " + err.Error()})
		v.Status = StatusFailed
		return v
	}

	pubkey, err := ident.PublicKey()
	if err != nil {
		return errVerdict("bluesky", "no atproto signing key in DID document: "+err.Error())
	}

	sigErr := commit.VerifySignature(pubkey)
	v.Checks = append(v.Checks, check("commit_signature", sigErr == nil,
		"repo commit is signed by the DID's atproto key"))
	if sigErr != nil {
		v.Status = StatusFailed
		return v
	}

	// MST inclusion: GetRecordCID walks the MST from the (now-verified) commit
	// root. Success proves the record is in the signed tree.
	cidPtr, recErr := r.GetRecordCID(ctx, syntax.NSID(collection), syntax.RecordKey(rkey))
	included := recErr == nil && cidPtr != nil
	v.Checks = append(v.Checks, check("mst_inclusion", included,
		"record is included in the signed commit's Merkle tree"))
	if !included {
		v.Status = StatusFailed
		return v
	}

	// Display-only excerpt. GetRecordBytes does not re-check the record block
	// against its CID, so this text is NOT cryptographically pinned — it never
	// influences the verdict (set after StatusVerified is decided).
	v.Content = &Excerpt{Kind: collection}
	if recBytes, _, err := r.GetRecordBytes(ctx, syntax.NSID(collection), syntax.RecordKey(rkey)); err == nil {
		v.Content.Text = extractBskyText(recBytes)
	}

	v.Status = StatusVerified
	if exp := strings.TrimSpace(in.Expected); exp != "" {
		v.Expected = matchBlueskyExpected(exp, ident)
	}
	return v
}

// fetchCAR retrieves the record + proof CAR from the PDS sync endpoint.
func (bv *BlueskyVerifier) fetchCAR(ctx context.Context, pds, did, collection, rkey string) ([]byte, error) {
	endpoint := strings.TrimRight(pds, "/") + "/xrpc/com.atproto.sync.getRecord"
	q := url.Values{"did": {did}, "collection": {collection}, "rkey": {rkey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := bv.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("PDS returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

// matchBlueskyExpected compares an expected handle or DID to the resolved identity.
func matchBlueskyExpected(exp string, ident *identity.Identity) *Match {
	e := strings.TrimPrefix(strings.TrimSpace(exp), "@")
	if strings.HasPrefix(e, "did:") {
		return &Match{Provided: exp, Matches: strings.EqualFold(e, ident.DID.String())}
	}
	return &Match{Provided: exp, Matches: strings.EqualFold(e, ident.Handle.String())}
}

// parseBlueskyRef extracts (authority, collection, rkey) from an at:// URI or a
// bsky.app post URL.
func parseBlueskyRef(raw string) (authority, collection, rkey string, err error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "at://") {
		aturi, perr := syntax.ParseATURI(s)
		if perr != nil {
			return "", "", "", fmt.Errorf("invalid at:// URI: %w", perr)
		}
		coll := aturi.Collection().String()
		rk := aturi.RecordKey().String()
		if coll == "" || rk == "" {
			return "", "", "", fmt.Errorf("at:// URI must include collection and record key")
		}
		return aturi.Authority().String(), coll, rk, nil
	}
	u, perr := url.Parse(s)
	if perr != nil {
		return "", "", "", fmt.Errorf("invalid URL: %w", perr)
	}
	// Expected path: /profile/{authority}/post/{rkey}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 4 && parts[0] == "profile" && parts[2] == "post" {
		return parts[1], "app.bsky.feed.post", parts[3], nil
	}
	return "", "", "", fmt.Errorf("unrecognized Bluesky URL: expected /profile/<id>/post/<rkey>")
}

// extractBskyText pulls the "text" field from a DAG-CBOR record without a full
// schema decode. Display-only; NOT part of the cryptographic verification.
func extractBskyText(record []byte) string {
	var m map[string]any
	if err := cbornode.DecodeInto(record, &m); err != nil {
		return ""
	}
	if text, ok := m["text"].(string); ok {
		return text
	}
	return ""
}

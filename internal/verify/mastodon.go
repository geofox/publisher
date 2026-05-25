package verify

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gowebpki/jcs"
	"github.com/mr-tron/base58"
)

// MastodonVerifier verifies an ActivityPub object by URL — used for both
// Mastodon and Threads, which are both ActivityPub/fediverse. Default assurance
// is "origin" (the actor's domain is authoritative for the object); if a valid
// FEP-8b32 eddsa-jcs-2022 integrity proof is present, assurance becomes
// "cryptographic". Threads (Meta) does not emit such proofs, so Threads posts
// stay at "origin". The platform field is only the verdict label.
type MastodonVerifier struct {
	platform string // verdict label: "mastodon" or "threads"
	http     *http.Client
}

// NewMastodonVerifier builds an ActivityPub verifier labeled "mastodon".
func NewMastodonVerifier(timeout time.Duration) *MastodonVerifier {
	return &MastodonVerifier{platform: "mastodon", http: NewSafeClient(timeout)}
}

// NewThreadsVerifier builds an ActivityPub verifier labeled "threads". Threads
// has no native per-post signature, so this only confirms origin authority for
// fediverse-federated Threads accounts (origin assurance, never cryptographic).
func NewThreadsVerifier(timeout time.Duration) *MastodonVerifier {
	return &MastodonVerifier{platform: "threads", http: NewSafeClient(timeout)}
}

type apObject struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	AttributedTo string          `json:"attributedTo"`
	Content      string          `json:"content"`
	Published    string          `json:"published"`
	Proof        json.RawMessage `json:"proof"`
}

type apActor struct {
	ID                string          `json:"id"`
	PreferredUsername string          `json:"preferredUsername"`
	AssertionMethod   json.RawMessage `json:"assertionMethod"`
}

func (mv *MastodonVerifier) Verify(ctx context.Context, in Input) Verdict {
	v := Verdict{Platform: mv.platform, Assurance: "origin", Checks: []Check{}}

	objRaw, err := mv.fetchAPRaw(ctx, strings.TrimSpace(in.Raw))
	if err != nil {
		return errVerdict(mv.platform, "could not fetch object: "+err.Error())
	}
	if looksLikeHTML(objRaw) {
		return errVerdict(mv.platform, htmlNotAPMessage(mv.platform))
	}
	var obj apObject
	if err := json.Unmarshal(objRaw, &obj); err != nil {
		return errVerdict(mv.platform, "could not parse object: "+err.Error())
	}
	if obj.AttributedTo == "" || obj.ID == "" {
		return errVerdict(mv.platform, "object missing id/attributedTo")
	}

	actorRaw, err := mv.fetchAPRaw(ctx, obj.AttributedTo)
	if err != nil {
		return errVerdict(mv.platform, "could not fetch actor: "+err.Error())
	}
	if looksLikeHTML(actorRaw) {
		return errVerdict(mv.platform, "the actor URL returned an HTML page, not ActivityPub")
	}
	var actor apActor
	if err := json.Unmarshal(actorRaw, &actor); err != nil {
		return errVerdict(mv.platform, "could not parse actor: "+err.Error())
	}

	objHost := hostOf(obj.ID)
	actorHost := hostOf(actor.ID)
	originOK := objHost != "" && objHost == actorHost && actorHost == hostOf(obj.AttributedTo)
	v.Checks = append(v.Checks, check("origin_authority", originOK,
		"object id and actor share an authoritative host"))

	acct := actor.PreferredUsername + "@" + actorHost
	v.Signer = &Signer{ActorURI: actor.ID, Acct: acct, OriginHost: actorHost}
	v.Content = &Excerpt{Text: stripHTML(obj.Content), CreatedAt: obj.Published}

	if !originOK {
		v.Status = StatusFailed
		if exp := strings.TrimSpace(in.Expected); exp != "" {
			v.Expected = &Match{Provided: exp, Matches: false, Detail: "origin authority failed"}
		}
		return v
	}

	// FEP-8b32 object integrity proof, if present.
	if len(obj.Proof) > 0 && string(obj.Proof) != "null" {
		ok, suite, detail := verifyIntegrityProof(objRaw, obj.Proof, actor)
		switch {
		case ok:
			v.Checks = append(v.Checks, Check{Name: "integrity_proof", Result: "pass", Detail: detail})
			v.Assurance = "cryptographic"
		case suite != "eddsa-jcs-2022":
			v.Checks = append(v.Checks, Check{Name: "integrity_proof", Result: "skip", Detail: detail})
			v.Warnings = append(v.Warnings, "integrity proof present but cryptosuite unsupported; origin assurance only")
		default:
			v.Checks = append(v.Checks, Check{Name: "integrity_proof", Result: "fail", Detail: detail})
			v.Status = StatusFailed
			return v
		}
	}

	v.Status = StatusVerified
	if exp := strings.TrimSpace(in.Expected); exp != "" {
		want := strings.TrimPrefix(strings.TrimSpace(exp), "@")
		v.Expected = &Match{Provided: exp, Matches: strings.EqualFold(want, acct)}
	}
	return v
}

// fetchAPRaw GETs an ActivityPub document with AP content negotiation and
// returns its raw bytes. The bytes are needed verbatim for FEP-8b32
// canonicalization (reparsing through a typed struct would drop fields and break
// the signature hash), so callers unmarshal the result themselves.
func (mv *MastodonVerifier) fetchAPRaw(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/activity+json, application/ld+json")
	resp, err := mv.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
}

type integrityProof struct {
	Type               string `json:"type"`
	Cryptosuite        string `json:"cryptosuite"`
	VerificationMethod string `json:"verificationMethod"`
	ProofPurpose       string `json:"proofPurpose"`
	Created            string `json:"created"`
	ProofValue         string `json:"proofValue"`
}

// verifyIntegrityProof implements FEP-8b32 verification for the eddsa-jcs-2022
// cryptosuite: hashData = sha256(JCS(proofOptions)) || sha256(JCS(documentWithoutProof));
// then ed25519.Verify(key, hashData, signature). objRaw MUST be the original
// document bytes from the server. Returns (ok, cryptosuite, detail).
func verifyIntegrityProof(objRaw []byte, proofRaw json.RawMessage, actor apActor) (bool, string, string) {
	var p integrityProof
	if err := json.Unmarshal(proofRaw, &p); err != nil {
		return false, "", "unparseable proof: " + err.Error()
	}
	if p.Cryptosuite != "eddsa-jcs-2022" {
		return false, p.Cryptosuite, "unsupported cryptosuite: " + p.Cryptosuite
	}
	if p.ProofPurpose != "assertionMethod" {
		return false, p.Cryptosuite, "proof purpose must be assertionMethod, got: " + p.ProofPurpose
	}

	pubkey, err := resolveAssertionKey(actor, p.VerificationMethod)
	if err != nil {
		return false, p.Cryptosuite, err.Error()
	}

	sig, err := decodeMultibase(p.ProofValue)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false, p.Cryptosuite, "bad proofValue"
	}

	// proofOptions = the proof object without proofValue.
	proofOpts := map[string]any{}
	_ = json.Unmarshal(proofRaw, &proofOpts)
	delete(proofOpts, "proofValue")
	proofCanon, err := canonicalize(proofOpts)
	if err != nil {
		return false, p.Cryptosuite, "canonicalize proof: " + err.Error()
	}

	// document = the FULL original object (raw bytes) without its proof property.
	// Canonicalizing a reparsed struct subset would drop fields and never match.
	docMap := map[string]any{}
	if err := json.Unmarshal(objRaw, &docMap); err != nil {
		return false, p.Cryptosuite, "reparse document: " + err.Error()
	}
	delete(docMap, "proof")
	docCanon, err := canonicalize(docMap)
	if err != nil {
		return false, p.Cryptosuite, "canonicalize document: " + err.Error()
	}

	proofHash := sha256.Sum256(proofCanon)
	docHash := sha256.Sum256(docCanon)
	hashData := append(proofHash[:], docHash[:]...)

	if ed25519.Verify(pubkey, hashData, sig) {
		return true, p.Cryptosuite, "eddsa-jcs-2022 proof valid"
	}
	return false, p.Cryptosuite, "ed25519 verification failed"
}

// canonicalize returns the JCS (RFC 8785) canonical form of v.
func canonicalize(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

// resolveAssertionKey finds the verificationMethod key in the actor's
// assertionMethod and returns its ed25519 public key. assertionMethod may be a
// single embedded Multikey object or an array of them. NOTE: a bare string
// DID-URL reference form is not resolved (returns not-found → StatusFailed);
// Mastodon and current fediverse software embed the Multikey inline.
func resolveAssertionKey(actor apActor, vm string) (ed25519.PublicKey, error) {
	type multikey struct {
		ID                 string `json:"id"`
		PublicKeyMultibase string `json:"publicKeyMultibase"`
	}
	var methods []multikey
	if len(actor.AssertionMethod) > 0 {
		if err := json.Unmarshal(actor.AssertionMethod, &methods); err != nil {
			var one multikey
			if err2 := json.Unmarshal(actor.AssertionMethod, &one); err2 == nil {
				methods = []multikey{one}
			}
		}
	}
	for _, m := range methods {
		if m.ID == vm && m.PublicKeyMultibase != "" {
			return decodeMultikeyEd25519(m.PublicKeyMultibase)
		}
	}
	return nil, fmt.Errorf("verificationMethod %q not found in actor assertionMethod", vm)
}

// decodeMultibase decodes a 'z' base58btc multibase string to raw bytes.
func decodeMultibase(s string) ([]byte, error) {
	if !strings.HasPrefix(s, "z") {
		return nil, fmt.Errorf("unsupported multibase prefix")
	}
	return base58.Decode(s[1:])
}

// decodeMultikeyEd25519 decodes a Multikey (multibase + 0xed01 multicodec
// prefix) to a 32-byte ed25519 public key.
func decodeMultikeyEd25519(mb string) (ed25519.PublicKey, error) {
	raw, err := decodeMultibase(mb)
	if err != nil {
		return nil, err
	}
	if len(raw) != 2+ed25519.PublicKeySize || raw[0] != 0xed || raw[1] != 0x01 {
		return nil, fmt.Errorf("not an ed25519 multikey")
	}
	return ed25519.PublicKey(raw[2:]), nil
}

// htmlNotAPMessage explains why an HTML response means we can't verify, tuned
// per platform. Threads is a dead end (Meta gates AP behind authenticated
// federation); for generic ActivityPub the AP object URL is worth trying.
func htmlNotAPMessage(platform string) string {
	if platform == "threads" {
		return "Threads doesn't expose posts as fetchable ActivityPub objects to " +
			"unauthenticated clients — Meta gates federation behind authenticated, signed " +
			"requests, so Threads posts can't be verified here"
	}
	return "the URL returned an HTML page, not ActivityPub — this post isn't exposed as a " +
		"fediverse/ActivityPub object (paste the ActivityPub object URL if you have it)"
}

// looksLikeHTML reports whether b appears to be an HTML page rather than an
// ActivityPub/JSON document. Servers that ignore AP content negotiation (Threads,
// and many post permalinks) return their web page, which would otherwise surface
// as a cryptic "invalid character '<'" JSON error.
func looksLikeHTML(b []byte) bool {
	t := bytes.TrimSpace(b)
	return len(t) > 0 && t[0] == '<'
}

// hostOf returns the lowercase host of a URL, or "" on parse failure.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Host)
}

// stripHTML removes tags from AP content for a plain-text excerpt.
func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

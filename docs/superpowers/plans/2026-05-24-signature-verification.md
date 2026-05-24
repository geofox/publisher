# Signature Verification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a credential-free tool that verifies whether a Nostr event (pasted JSON) or a Bluesky/Mastodon post (by URL) is authentically signed by the claimed author, reporting the actual signer with an explicit assurance level.

**Architecture:** A new isolated `internal/verify` package with a uniform `Verdict` result, a dispatcher that auto-detects the platform, and three credential-free verifiers (Nostr offline; Bluesky full commit-signature + MST-inclusion proof via the indigo atproto SDK; Mastodon origin-authority + FEP-8b32 JCS proof). Exposed via a new `POST /api/verify` endpoint and a "verify" tab in the embedded SPA. All outbound fetches go through an SSRF-guarded HTTP client.

**Tech Stack:** Go 1.25, `fiatjaf.com/nostr` (already a dep), `github.com/bluesky-social/indigo/atproto/*` (new), `github.com/gowebpki/jcs` + `github.com/mr-tron/base58` (new, FEP-8b32), Go stdlib `crypto/ed25519`, vanilla-JS SPA.

**Spec:** `docs/superpowers/specs/2026-05-24-signature-verification-design.md`

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/verify/verify.go` | `Verdict`/`Check`/`Signer`/`Match`/`Excerpt`/`Input`/`Status` types, `Verifier` interface, `Service` dispatcher + platform auto-detection |
| `internal/verify/safehttp.go` | SSRF-guarded `*http.Client` builder + `isBlockedIP` predicate |
| `internal/verify/nostr.go` | Nostr event verifier (offline) + expected-identity resolution (npub/hex/NIP-05) |
| `internal/verify/bluesky.go` | ATProto verifier: URL parse, DID lookup, CAR fetch, commit-signature + MST-inclusion |
| `internal/verify/mastodon.go` | ActivityPub verifier: AP fetch, origin authority, FEP-8b32 (eddsa-jcs-2022) |
| `internal/verify/*_test.go` | Unit + fixture tests per file |
| `internal/verify/testdata/` | Captured CAR, DID-doc, AP-object, actor fixtures |
| `internal/api/api.go` | `handleVerify` + route registration + `Verifier` field on `API` |
| `internal/config/config.go` | `PLCDirectoryURL`, `VerifyHTTPTimeout` config fields |
| `cmd/publisher/main.go` | Construct `verify.Service`, inject into `API` |
| `internal/web/assets/index.html` | "verify" tab + section markup |
| `internal/web/assets/verify.js` | Verify tab behavior |
| `internal/web/assets/main.js` | Register verify module |
| `internal/web/assets/app.css` | Verify result-card styles |
| `README.md` | Document `/api/verify` + verify tab + new env vars |

---

## Task 1: Core types and Verifier interface

**Files:**
- Create: `internal/verify/verify.go`
- Test: `internal/verify/verify_test.go`

- [ ] **Step 1: Write the failing test**

```go
package verify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerdictJSONOmitsEmpty(t *testing.T) {
	v := Verdict{Platform: "nostr", Status: StatusVerified, Assurance: "cryptographic",
		Checks: []Check{{Name: "signature_valid", Result: "pass"}}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"status":"verified"`) {
		t.Errorf("missing status: %s", s)
	}
	if strings.Contains(s, `"error"`) {
		t.Errorf("empty error should be omitted: %s", s)
	}
	if strings.Contains(s, `"signer"`) {
		t.Errorf("nil signer should be omitted: %s", s)
	}
}

func TestStatusConstants(t *testing.T) {
	if StatusVerified != "verified" || StatusFailed != "failed" || StatusError != "error" {
		t.Fatal("status constant values changed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/verify/ -run TestVerdict -v`
Expected: FAIL — `undefined: Verdict` (package does not compile).

- [ ] **Step 3: Write minimal implementation**

```go
// Package verify provides credential-free verification of social-media posts:
// it reports who actually signed a Nostr event or a Bluesky/Mastodon post, with
// an explicit assurance level, and optionally matches that signer against an
// expected identity. It never accesses the owner's signing key or credentials.
package verify

import "context"

// Status is the tri-state outcome of a verification.
//
//   - StatusVerified: authentic — the correct key signed this.
//   - StatusFailed:   the check completed and the signature/inclusion is INVALID
//     (tampering or impersonation).
//   - StatusError:    the check could NOT complete (network, unresolvable
//     identity, deleted post, malformed input). NOT evidence of forgery.
type Status string

const (
	StatusVerified Status = "verified"
	StatusFailed   Status = "failed"
	StatusError    Status = "error"
)

// Input is the user-supplied verification request.
type Input struct {
	Raw      string // pasted event JSON | post URL | at:// URI
	Platform string // optional explicit override: nostr|bluesky|mastodon
	Expected string // optional expected identity to match the signer against
}

// Verdict is the uniform result returned for every platform.
type Verdict struct {
	Platform  string   `json:"platform"`
	Status    Status   `json:"status"`
	Assurance string   `json:"assurance,omitempty"` // "cryptographic" | "origin"
	Signer    *Signer  `json:"signer,omitempty"`
	Expected  *Match   `json:"expected,omitempty"`
	Content   *Excerpt `json:"content,omitempty"`
	Checks    []Check  `json:"checks"`
	Warnings  []string `json:"warnings,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// Check is one transparent, named sub-step of a verification.
type Check struct {
	Name   string `json:"name"`
	Result string `json:"result"` // "pass" | "fail" | "skip"
	Detail string `json:"detail,omitempty"`
}

// Signer carries platform-specific identity fields; unused fields are omitted.
type Signer struct {
	PubkeyHex string `json:"pubkey_hex,omitempty"` // nostr
	Npub      string `json:"npub,omitempty"`       // nostr

	DID            string `json:"did,omitempty"`             // bluesky
	Handle         string `json:"handle,omitempty"`          // bluesky
	HandleVerified *bool  `json:"handle_verified,omitempty"` // bluesky
	PDS            string `json:"pds,omitempty"`             // bluesky

	ActorURI   string `json:"actor_uri,omitempty"`   // mastodon
	Acct       string `json:"acct,omitempty"`        // mastodon (user@domain)
	OriginHost string `json:"origin_host,omitempty"` // mastodon
}

// Match reports the result of comparing the signer to a user-supplied identity.
type Match struct {
	Provided string `json:"provided"`
	Matches  bool   `json:"matches"`
	Detail   string `json:"detail,omitempty"`
}

// Excerpt is a small view of the content that was verified.
type Excerpt struct {
	Text      string `json:"text,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Kind      string `json:"kind,omitempty"` // nostr kind / bsky collection
}

// Verifier verifies one platform's posts.
type Verifier interface {
	Verify(ctx context.Context, in Input) Verdict
}

// errVerdict is a helper to build a StatusError verdict.
func errVerdict(platform, msg string) Verdict {
	return Verdict{Platform: platform, Status: StatusError, Checks: []Check{}, Error: msg}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/verify/ -run 'TestVerdict|TestStatus' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add internal/verify/verify.go internal/verify/verify_test.go
git commit -m "verify: add core Verdict/Input types and Verifier interface"
```

---

## Task 2: SSRF-guarded HTTP client

**Files:**
- Create: `internal/verify/safehttp.go`
- Test: `internal/verify/safehttp_test.go`

- [ ] **Step 1: Write the failing test**

```go
package verify

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1":   true,  // loopback
		"::1":         true,  // loopback v6
		"10.0.0.5":    true,  // private
		"192.168.1.1": true,  // private
		"172.16.0.1":  true,  // private
		"169.254.0.1": true,  // link-local
		"0.0.0.0":     true,  // unspecified
		"fc00::1":     true,  // ULA v6
		"8.8.8.8":     false, // public
		"1.1.1.1":     false, // public
	}
	for s, want := range cases {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		if got := isBlockedIP(ip); got != want {
			t.Errorf("isBlockedIP(%s) = %v, want %v", s, got, want)
		}
	}
}

func TestSafeClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := newSafeClient(2 * time.Second)
	_, err := c.Get(srv.URL) // srv.URL is http://127.0.0.1:PORT
	if err == nil {
		t.Fatal("expected loopback dial to be blocked, got nil error")
	}
}

func TestSafeClientContextDeadline(t *testing.T) {
	c := newSafeClient(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://8.8.8.8", nil)
	if _, err := c.Do(req); err == nil {
		t.Fatal("expected context deadline error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/verify/ -run 'IsBlockedIP|SafeClient' -v`
Expected: FAIL — `undefined: isBlockedIP` / `undefined: newSafeClient`.

- [ ] **Step 3: Write minimal implementation**

```go
package verify

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// maxRedirects bounds redirect chains so a hostile server can't spin us forever.
const maxRedirects = 5

// maxResponseBytes caps any verification response body (CAR files, AP objects).
const maxResponseBytes int64 = 8 << 20 // 8 MB

// isBlockedIP reports whether an IP is in a range we must never dial from a
// user-controlled URL: loopback, RFC1918/ULA private, link-local, or the
// unspecified address. This is the core SSRF guard.
func isBlockedIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// newSafeClient builds an http.Client whose dialer rejects any connection to a
// blocked IP. The check runs in the dialer Control hook, which fires AFTER DNS
// resolution on the concrete IP about to be dialed — so it also defeats DNS
// rebinding (a hostname that resolves to a public IP first and a private IP on
// a later lookup is still checked per-connection).
func newSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: timeout,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("could not parse dial address %q", host)
			}
			if isBlockedIP(ip) {
				return fmt.Errorf("blocked non-public address %s", ip)
			}
			return nil
		},
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{DialContext: dialer.DialContext},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/verify/ -run 'IsBlockedIP|SafeClient' -v`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
git add internal/verify/safehttp.go internal/verify/safehttp_test.go
git commit -m "verify: add SSRF-guarded HTTP client"
```

---

## Task 3: Nostr verifier (offline) with expected-identity match

**Files:**
- Create: `internal/verify/nostr.go`
- Test: `internal/verify/nostr_test.go`

Add the indigo-free Nostr deps are already present (`fiatjaf.com/nostr`). This task also fetches NIP-05 via the safe client built in Task 2.

- [ ] **Step 1: Write the failing test**

Note: the known-good event below is a real, valid event from the `fiatjaf.com/nostr` test suite.

```go
package verify

import (
	"context"
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

// hasCheck reports whether v contains a check with the given name and result.
func hasCheck(v Verdict, name, result string) bool {
	for _, c := range v.Checks {
		if c.Name == name && c.Result == result {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/verify/ -run TestNostr -v`
Expected: FAIL — `undefined: NostrVerifier`.

- [ ] **Step 3: Write minimal implementation**

```go
package verify

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/nip19"
)

// NostrVerifier verifies a pasted Nostr event. Verification is fully offline;
// HTTP is used only to resolve an expected NIP-05 identity, when supplied.
type NostrVerifier struct {
	HTTP *http.Client // SSRF-guarded; used only for NIP-05 resolution
}

func (nv *NostrVerifier) Verify(ctx context.Context, in Input) Verdict {
	var ev nostr.Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(in.Raw)), &ev); err != nil {
		return errVerdict("nostr", "could not parse event JSON: "+err.Error())
	}

	v := Verdict{Platform: "nostr", Assurance: "cryptographic", Checks: []Check{}}

	idOK := ev.CheckID()
	v.Checks = append(v.Checks, check("id_matches", idOK,
		"recomputed event id matches the declared id"))

	sigOK := false
	if idOK {
		sigOK = ev.VerifySignature()
	}
	v.Checks = append(v.Checks, check("signature_valid", sigOK,
		"schnorr signature is valid for the event pubkey"))

	npub := nip19.EncodeNpub(ev.PubKey) // returns a single string (no error)
	v.Signer = &Signer{PubkeyHex: ev.PubKey.Hex(), Npub: npub}
	v.Content = &Excerpt{
		Text:      ev.Content,
		CreatedAt: ev.CreatedAt.Time().UTC().Format("2006-01-02T15:04:05Z"),
		Kind:      "kind " + strconv.Itoa(int(ev.Kind)),
	}

	if idOK && sigOK {
		v.Status = StatusVerified
	} else {
		v.Status = StatusFailed
	}

	if exp := strings.TrimSpace(in.Expected); exp != "" {
		v.Expected = nv.matchExpected(ctx, exp, ev.PubKey)
	}
	return v
}

// matchExpected resolves an expected identity (npub, 64-char hex, or NIP-05
// name@domain) to a pubkey and compares it to the event's pubkey.
func (nv *NostrVerifier) matchExpected(ctx context.Context, exp string, actual nostr.PubKey) *Match {
	want, err := nv.resolveExpected(ctx, exp)
	if err != nil {
		return &Match{Provided: exp, Matches: false, Detail: "could not resolve: " + err.Error()}
	}
	return &Match{Provided: exp, Matches: want == actual}
}

func (nv *NostrVerifier) resolveExpected(ctx context.Context, exp string) (nostr.PubKey, error) {
	switch {
	case strings.HasPrefix(exp, "npub1"):
		prefix, val, err := nip19.Decode(exp)
		if err != nil {
			return nostr.PubKey{}, err
		}
		if prefix != "npub" {
			return nostr.PubKey{}, fmt.Errorf("not an npub: %s", prefix)
		}
		return val.(nostr.PubKey), nil
	case len(exp) == 64 && isHex(exp):
		b, err := hex.DecodeString(exp)
		if err != nil {
			return nostr.PubKey{}, err
		}
		return nostr.PubKey(b), nil
	case strings.Contains(exp, "@"):
		pp, err := nip05.QueryIdentifier(ctx, exp)
		if err != nil {
			return nostr.PubKey{}, err
		}
		return pp.PublicKey, nil
	default:
		return nostr.PubKey{}, fmt.Errorf("unrecognized identity format")
	}
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// check builds a Check whose result is pass/fail from ok.
func check(name string, ok bool, detail string) Check {
	r := "fail"
	if ok {
		r = "pass"
	}
	return Check{Name: name, Result: r, Detail: detail}
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/verify/ -run TestNostr -v`
Expected: PASS (all six). `TestNostrExpectedNpubMatch` runs offline (npub decode). The NIP-05 path is exercised in Task-level integration only; no NIP-05 test makes a network call here.

- [ ] **Step 5: Commit**

```bash
git add internal/verify/nostr.go internal/verify/nostr_test.go
git commit -m "verify: add offline Nostr event verifier with expected-identity match"
```

---

## Task 4: Dispatcher + platform auto-detection

**Files:**
- Modify: `internal/verify/verify.go` (add `Service` + detection)
- Test: `internal/verify/dispatch_test.go`

- [ ] **Step 1: Write the failing test**

```go
package verify

import (
	"context"
	"testing"
)

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{goodEvent, "nostr"},
		{`  {"id":"x","pubkey":"y","sig":"z"}  `, "nostr"},
		{"at://did:plc:abc/app.bsky.feed.post/123", "bluesky"},
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "bluesky"},
		{"https://mastodon.social/@alice/111222333", "mastodon"},
		{"https://example.social/users/bob/statuses/999", "mastodon"},
	}
	for _, c := range cases {
		got, err := detectPlatform(c.raw)
		if err != nil {
			t.Errorf("detectPlatform(%q) error: %v", c.raw, err)
			continue
		}
		if got != c.want {
			t.Errorf("detectPlatform(%q) = %s, want %s", c.raw, got, c.want)
		}
	}
}

func TestDetectNeventRejected(t *testing.T) {
	_, err := detectPlatform("nevent1qqs...")
	if err == nil {
		t.Fatal("expected nevent to be rejected with guidance")
	}
}

type fakeVerifier struct{ platform string }

func (f fakeVerifier) Verify(_ context.Context, _ Input) Verdict {
	return Verdict{Platform: f.platform, Status: StatusVerified, Checks: []Check{}}
}

func TestServiceRoutesByDetection(t *testing.T) {
	s := &Service{
		Nostr:    fakeVerifier{"nostr"},
		Bluesky:  fakeVerifier{"bluesky"},
		Mastodon: fakeVerifier{"mastodon"},
	}
	v := s.Verify(context.Background(), Input{Raw: "https://bsky.app/profile/a/post/1"})
	if v.Platform != "bluesky" {
		t.Fatalf("routed to %s, want bluesky", v.Platform)
	}
}

func TestServiceExplicitOverride(t *testing.T) {
	s := &Service{Nostr: fakeVerifier{"nostr"}, Mastodon: fakeVerifier{"mastodon"}}
	v := s.Verify(context.Background(), Input{Raw: "anything", Platform: "mastodon"})
	if v.Platform != "mastodon" {
		t.Fatalf("override routed to %s, want mastodon", v.Platform)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/verify/ -run 'Detect|Service' -v`
Expected: FAIL — `undefined: detectPlatform` / `undefined: Service`.

- [ ] **Step 3: Append implementation to `verify.go`**

```go
import (
	"encoding/json"      // add to existing import block
	"fmt"
	"net/url"
	"strings"
)

// Service routes an Input to the right platform verifier.
type Service struct {
	Nostr    Verifier
	Bluesky  Verifier
	Mastodon Verifier
}

// Verify selects a verifier (explicit Platform override wins, else auto-detect)
// and runs it. Detection or routing failures return a StatusError verdict.
func (s *Service) Verify(ctx context.Context, in Input) Verdict {
	platform := strings.TrimSpace(strings.ToLower(in.Platform))
	if platform == "" {
		p, err := detectPlatform(in.Raw)
		if err != nil {
			return errVerdict("", err.Error())
		}
		platform = p
	}
	v := s.verifierFor(platform)
	if v == nil {
		return errVerdict(platform, "unsupported or unconfigured platform: "+platform)
	}
	return v.Verify(ctx, in)
}

func (s *Service) verifierFor(platform string) Verifier {
	switch platform {
	case "nostr":
		return s.Nostr
	case "bluesky":
		return s.Bluesky
	case "mastodon":
		return s.Mastodon
	default:
		return nil
	}
}

// detectPlatform infers the platform from the raw input.
func detectPlatform(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return "", fmt.Errorf("empty input")
	case strings.HasPrefix(s, "{"):
		var probe struct {
			ID     string `json:"id"`
			PubKey string `json:"pubkey"`
			Sig    string `json:"sig"`
		}
		if err := json.Unmarshal([]byte(s), &probe); err == nil && probe.PubKey != "" && probe.Sig != "" {
			return "nostr", nil
		}
		return "", fmt.Errorf("input looks like JSON but is not a Nostr event")
	case strings.HasPrefix(s, "nevent1"), strings.HasPrefix(s, "note1"):
		return "", fmt.Errorf("paste the full event JSON — nevent/note references are not supported")
	case strings.HasPrefix(s, "at://"):
		return "bluesky", nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		u, err := url.Parse(s)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %w", err)
		}
		host := strings.ToLower(u.Hostname())
		if host == "bsky.app" || strings.HasSuffix(host, ".bsky.app") {
			return "bluesky", nil
		}
		return "mastodon", nil
	default:
		return "", fmt.Errorf("unrecognized input: paste an event JSON or a post URL")
	}
}
```

Note: merge the new imports into the existing `import` block at the top of `verify.go` (which currently imports only `context`).

- [ ] **Step 4: Run tests**

Run: `go test ./internal/verify/ -run 'Detect|Service' -v`
Expected: PASS (all four).

- [ ] **Step 5: Run the whole package**

Run: `go test ./internal/verify/ -v`
Expected: PASS (Tasks 1–4).

- [ ] **Step 6: Commit**

```bash
git add internal/verify/verify.go internal/verify/dispatch_test.go
git commit -m "verify: add platform dispatcher with auto-detection"
```

---

## Task 5: Bluesky verifier (commit signature + MST inclusion)

**Files:**
- Create: `internal/verify/bluesky.go`
- Test: `internal/verify/bluesky_test.go`
- Add dependency: `github.com/bluesky-social/indigo`

This task wires the indigo atproto SDK. Unit tests cover URL parsing and verdict mapping with fakes; a network-gated integration test proves the full crypto path end-to-end (indigo's MST/commit crypto is upstream-tested, so our unit tests focus on our glue).

- [ ] **Step 1: Add the indigo dependency**

Run:
```bash
go get github.com/bluesky-social/indigo@v0.0.0-20260520161040-0eb7e0ea71bc
go mod tidy
```
Expected: `go.mod` gains `github.com/bluesky-social/indigo`; `go.sum` grows. Run `go build ./...` to confirm the module graph resolves.

- [ ] **Step 2: Write the failing test (URL parsing + integration)**

```go
package verify

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestParseBlueskyRef(t *testing.T) {
	cases := []struct {
		raw                              string
		authority, collection, recordKey string
		wantErr                          bool
	}{
		{"at://did:plc:ewvi7nxzyoun6zhxrhs64oiz/app.bsky.feed.post/3kabc", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "app.bsky.feed.post", "3kabc", false},
		{"https://bsky.app/profile/alice.bsky.social/post/3kabc", "alice.bsky.social", "app.bsky.feed.post", "3kabc", false},
		{"https://bsky.app/profile/did:plc:ewvi7nxzyoun6zhxrhs64oiz/post/3kxyz", "did:plc:ewvi7nxzyoun6zhxrhs64oiz", "app.bsky.feed.post", "3kxyz", false},
		{"https://bsky.app/profile/alice", "", "", "", true},
		{"https://example.com/foo", "", "", "", true},
	}
	for _, c := range cases {
		a, coll, rk, err := parseBlueskyRef(c.raw)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseBlueskyRef(%q) expected error", c.raw)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBlueskyRef(%q) error: %v", c.raw, err)
			continue
		}
		if a != c.authority || coll != c.collection || rk != c.recordKey {
			t.Errorf("parseBlueskyRef(%q) = (%q,%q,%q), want (%q,%q,%q)",
				c.raw, a, coll, rk, c.authority, c.collection, c.recordKey)
		}
	}
}

// TestBlueskyIntegration verifies a real public post end-to-end. Network-gated:
// set VERIFY_BSKY_URL to a real bsky.app post URL to run it.
//
//	VERIFY_BSKY_URL="https://bsky.app/profile/<handle>/post/<rkey>" go test ./internal/verify/ -run TestBlueskyIntegration -v
func TestBlueskyIntegration(t *testing.T) {
	raw := os.Getenv("VERIFY_BSKY_URL")
	if raw == "" {
		t.Skip("set VERIFY_BSKY_URL to run the Bluesky integration test")
	}
	bv := NewBlueskyVerifier("https://plc.directory", 15*time.Second)
	v := bv.Verify(context.Background(), Input{Raw: raw})
	if v.Status != StatusVerified {
		t.Fatalf("status = %s (err=%s) checks=%+v", v.Status, v.Error, v.Checks)
	}
	if v.Assurance != "cryptographic" {
		t.Errorf("assurance = %q", v.Assurance)
	}
	if !hasCheck(v, "commit_signature", "pass") || !hasCheck(v, "mst_inclusion", "pass") {
		t.Errorf("missing crypto checks: %+v", v.Checks)
	}
	if v.Signer == nil || v.Signer.DID == "" {
		t.Errorf("signer not populated: %+v", v.Signer)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/verify/ -run TestParseBluesky -v`
Expected: FAIL — `undefined: parseBlueskyRef` / `undefined: NewBlueskyVerifier`.

- [ ] **Step 4: Write the implementation**

```go
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
)

// BlueskyVerifier verifies an ATProto record by fetching the signed commit + MST
// proof from the author's PDS and checking both the commit signature (against the
// DID's atproto key) and the record's inclusion in the signed tree.
type BlueskyVerifier struct {
	dir  *identity.BaseDirectory
	http *http.Client
}

// NewBlueskyVerifier builds a verifier whose DID directory and PDS fetches both
// use the SSRF-guarded client. plcURL is the did:plc resolver base URL.
func NewBlueskyVerifier(plcURL string, timeout time.Duration) *BlueskyVerifier {
	client := newSafeClient(timeout)
	dir := &identity.BaseDirectory{
		PLCURL:     strings.TrimRight(plcURL, "/"),
		HTTPClient: *client,
	}
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

	v.Content = &Excerpt{Kind: collection}
	if text, _, err := r.GetRecordBytes(ctx, syntax.NSID(collection), syntax.RecordKey(rkey)); err == nil {
		v.Content.Text = extractBskyText(text)
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
// schema decode. The record bytes are CBOR; we do a best-effort UTF-8 scan for
// the text value, returning "" if not found. This is display-only; it is NOT
// part of the cryptographic verification.
func extractBskyText(record []byte) string {
	// app.bsky.feed.post stores text as a CBOR text string keyed by "text".
	// A robust decode is unnecessary for a display excerpt, so we locate the
	// "text" key and read the following CBOR text string header.
	i := bytes.Index(record, []byte("text"))
	if i < 0 || i+4 >= len(record) {
		return ""
	}
	return cborTextAfter(record[i+4:])
}

// cborTextAfter reads a CBOR text string (major type 3) at the start of b,
// returning its UTF-8 contents or "" if b does not begin with a text string.
func cborTextAfter(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	major := b[0] >> 5
	if major != 3 {
		return ""
	}
	n := int(b[0] & 0x1f)
	off := 1
	switch n {
	case 24:
		if len(b) < 2 {
			return ""
		}
		n, off = int(b[1]), 2
	case 25:
		if len(b) < 3 {
			return ""
		}
		n, off = int(b[1])<<8|int(b[2]), 3
	}
	if n < 0 || off+n > len(b) {
		return ""
	}
	return string(b[off : off+n])
}
```

- [ ] **Step 5: Run the URL-parsing test**

Run: `go test ./internal/verify/ -run TestParseBluesky -v`
Expected: PASS.

- [ ] **Step 6: Run the integration test against a real post**

Find any real public Bluesky post URL (open bsky.app, copy a post link). Then:
```bash
VERIFY_BSKY_URL="https://bsky.app/profile/<handle>/post/<rkey>" \
  go test ./internal/verify/ -run TestBlueskyIntegration -v
```
Expected: PASS — `commit_signature` and `mst_inclusion` both pass, `Status=verified`.
If `GetRecordBytes`/`GetRecordCID` signatures differ in the pinned indigo version, run `go doc github.com/bluesky-social/indigo/atproto/repo Repo` and adjust the calls.

- [ ] **Step 7: Run the full package (unit only)**

Run: `go test ./internal/verify/ -v`
Expected: PASS; `TestBlueskyIntegration` shows SKIP without the env var.

- [ ] **Step 8: Commit**

```bash
git add internal/verify/bluesky.go internal/verify/bluesky_test.go go.mod go.sum
git commit -m "verify: add Bluesky commit-signature + MST-inclusion verifier"
```

---

## Task 6: Mastodon verifier (origin authority + FEP-8b32)

**Files:**
- Create: `internal/verify/mastodon.go`
- Test: `internal/verify/mastodon_test.go`
- Add dependencies: `github.com/gowebpki/jcs`, `github.com/mr-tron/base58`

- [ ] **Step 1: Add dependencies**

Run:
```bash
go get github.com/gowebpki/jcs
go get github.com/mr-tron/base58
go mod tidy
```
Expected: both appear in `go.mod`. (`mr-tron/base58` is already in the graph via indigo; this promotes it to a direct dependency.)

- [ ] **Step 2: Write the failing test**

```go
package verify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// apFixtureServer serves an AP object at /note and an actor at /actor, both on
// the same host (so origin authority holds). Returns the note URL.
func apFixtureServer(t *testing.T, withProof bool) (*httptest.Server, string) {
	t.Helper()
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

func writeJSONLD(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/activity+json")
	_ = json.NewEncoder(w).Encode(v)
}

func mastodonVerifier() *MastodonVerifier {
	mv := NewMastodonVerifier(time.Second)
	// Tests serve on 127.0.0.1; allow loopback ONLY in tests by swapping the
	// client for a default one. Production uses the SSRF-guarded client.
	mv.http = &http.Client{Timeout: time.Second}
	return mv
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
```

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/verify/ -run TestMastodon -v`
Expected: FAIL — `undefined: NewMastodonVerifier`.

- [ ] **Step 4: Write the implementation**

```go
package verify

import (
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

// MastodonVerifier verifies an ActivityPub object by URL. Default assurance is
// "origin" (the actor's domain is authoritative for the object); if a valid
// FEP-8b32 eddsa-jcs-2022 integrity proof is present, assurance becomes
// "cryptographic".
type MastodonVerifier struct {
	http *http.Client
}

func NewMastodonVerifier(timeout time.Duration) *MastodonVerifier {
	return &MastodonVerifier{http: newSafeClient(timeout)}
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
	v := Verdict{Platform: "mastodon", Assurance: "origin", Checks: []Check{}}

	objRaw, err := mv.fetchAPRaw(ctx, strings.TrimSpace(in.Raw))
	if err != nil {
		return errVerdict("mastodon", "could not fetch object: "+err.Error())
	}
	var obj apObject
	if err := json.Unmarshal(objRaw, &obj); err != nil {
		return errVerdict("mastodon", "could not parse object: "+err.Error())
	}
	if obj.AttributedTo == "" || obj.ID == "" {
		return errVerdict("mastodon", "object missing id/attributedTo")
	}

	actorRaw, err := mv.fetchAPRaw(ctx, obj.AttributedTo)
	if err != nil {
		return errVerdict("mastodon", "could not fetch actor: "+err.Error())
	}
	var actor apActor
	if err := json.Unmarshal(actorRaw, &actor); err != nil {
		return errVerdict("mastodon", "could not parse actor: "+err.Error())
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
// string, an object, or an array of either.
func resolveAssertionKey(actor apActor, vm string) (ed25519.PublicKey, error) {
	type multikey struct {
		ID                 string `json:"id"`
		PublicKeyMultibase string `json:"publicKeyMultibase"`
	}
	var methods []multikey
	if len(actor.AssertionMethod) > 0 {
		// try array of objects
		if err := json.Unmarshal(actor.AssertionMethod, &methods); err != nil {
			// try single object
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
```

- [ ] **Step 5: Run tests**

Run: `go test ./internal/verify/ -run TestMastodon -v`
Expected: PASS (origin verified, origin mismatch fails, expected-acct match).

- [ ] **Step 6: Run full package**

Run: `go test ./internal/verify/ -v`
Expected: PASS (integration tests SKIP).

- [ ] **Step 7: Commit**

```bash
git add internal/verify/mastodon.go internal/verify/mastodon_test.go go.mod go.sum
git commit -m "verify: add Mastodon origin-authority + FEP-8b32 verifier"
```

---

## Task 7: API endpoint `POST /api/verify`

**Files:**
- Modify: `internal/api/api.go` (add field, route, handler)
- Test: `internal/api/verify_test.go`

- [ ] **Step 1: Write the failing test**

```go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/verify"
)

type stubVerifier struct{ v verify.Verdict }

func (s stubVerifier) Verify(_ context.Context, _ verify.Input) verify.Verdict { return s.v }

func postVerify(t *testing.T, a *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/verify", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestHandleVerifyVerified(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "nostr", Status: verify.StatusVerified, Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out verify.Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != verify.StatusVerified {
		t.Errorf("status = %s", out.Status)
	}
}

func TestHandleVerifyFailedIs200(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "nostr", Status: verify.StatusFailed, Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed verdict should still be 200, got %d", rec.Code)
	}
}

func TestHandleVerifyErrorIs502(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "bluesky", Status: verify.StatusError, Error: "network", Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("error verdict should be 502, got %d", rec.Code)
	}
}

func TestHandleVerifyEmptyInputIs400(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{}}}
	rec := postVerify(t, a, `{"input":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty input should be 400, got %d", rec.Code)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/api/ -run TestHandleVerify -v`
Expected: FAIL — `API` has no field `Verify`.

- [ ] **Step 3: Add the import, struct field, route, and handler**

In `internal/api/api.go`, add to the import block:
```go
	"github.com/geofox/publisher/internal/verify"
```

Add a `Verifier` interface near the `Syncer` interface:
```go
// Verifier is implemented by *verify.Service; extracted so the api package can
// be tested with a stub and has no hard dependency on the concrete dispatcher.
type Verifier interface {
	Verify(ctx context.Context, in verify.Input) verify.Verdict
}
```

Add a field to the `API` struct (after `HomeRelay`):
```go
	Verify Verifier // set by cmd/publisher; verifies pasted events / post URLs
```

Register the route in `Routes()` (after the sync routes, before `mux.Handle("/", ...)`):
```go
	mux.HandleFunc("POST /api/verify", a.handleVerify)
```

Add the handler (place it after the sync handlers):
```go
// ─── POST /api/verify ────────────────────────────────────────────────────

func (a *API) handleVerify(w http.ResponseWriter, r *http.Request) {
	if a.Verify == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "verification not configured")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512<<10) // 512 KB
	var req struct {
		Input    string `json:"input"`
		Platform string `json:"platform"`
		Expected string `json:"expected"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "input is required")
		return
	}
	v := a.Verify.Verify(r.Context(), verify.Input{
		Raw: req.Input, Platform: req.Platform, Expected: req.Expected,
	})
	status := http.StatusOK
	if v.Status == verify.StatusError {
		status = http.StatusBadGateway
	}
	httpx.WriteJSON(w, status, v)
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/api/ -run TestHandleVerify -v`
Expected: PASS (all four).

- [ ] **Step 5: Commit**

```bash
git add internal/api/api.go internal/api/verify_test.go
git commit -m "api: add POST /api/verify endpoint"
```

---

## Task 8: Config fields

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go` (add cases; create if absent)

The existing `config.go` uses a `getEnv(k, d string) string` helper and parses
durations inline with `time.ParseDuration(getEnv(...))` (there is no
`getDuration` helper). `Load()` builds a `Config` value named `c`, requires
`NSEC_HEX`/`OWNER_PUBKEY`/`BLOSSOM_URL`, and validates that `OWNER_PUBKEY` equals
the pubkey derived from `NSEC_HEX`. The test below uses a real matching keypair
(secret `00…01` → its secp256k1 x-only pubkey).

- [ ] **Step 1: Write the failing test**

```go
package config

import (
	"testing"
	"time"
)

func TestVerifyConfigDefaults(t *testing.T) {
	t.Setenv("PLC_DIRECTORY_URL", "")
	t.Setenv("VERIFY_HTTP_TIMEOUT", "")
	// Real matching keypair: secret 00..01, its derived x-only pubkey.
	t.Setenv("NSEC_HEX", "0000000000000000000000000000000000000000000000000000000000000001")
	t.Setenv("OWNER_PUBKEY", "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798")
	t.Setenv("BLOSSOM_URL", "https://blossom.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.PLCDirectoryURL != "https://plc.directory" {
		t.Errorf("PLCDirectoryURL default = %q", cfg.PLCDirectoryURL)
	}
	if cfg.VerifyHTTPTimeout != 10*time.Second {
		t.Errorf("VerifyHTTPTimeout default = %v", cfg.VerifyHTTPTimeout)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestVerifyConfig -v`
Expected: FAIL — `cfg.PLCDirectoryURL` undefined.

- [ ] **Step 3: Add the fields and parsing**

Add to the `Config` struct (e.g. after `SyncRelaysDefault`):
```go
	PLCDirectoryURL   string
	VerifyHTTPTimeout time.Duration
```

In `Load()`, set `PLCDirectoryURL` in the opening `Config{...}` literal alongside
the other `getEnv` fields:
```go
		PLCDirectoryURL: getEnv("PLC_DIRECTORY_URL", "https://plc.directory"),
```

And add the duration to the block of `time.ParseDuration` calls (after the
`ScheduleGrace` line), matching the existing error-handling style:
```go
	if c.VerifyHTTPTimeout, err = time.ParseDuration(getEnv("VERIFY_HTTP_TIMEOUT", "10s")); err != nil {
		return c, fmt.Errorf("VERIFY_HTTP_TIMEOUT: %w", err)
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "config: add PLC_DIRECTORY_URL and VERIFY_HTTP_TIMEOUT"
```

---

## Task 9: Wire the verifier in `cmd/publisher/main.go`

**Files:**
- Modify: `cmd/publisher/main.go`

In `cmd/publisher/main.go`, the API value is the variable `a` (`a := api.New(np, mp)`),
and dependencies are assigned directly (`a.Store = st`, `a.Dispatch = d`,
`a.Sync = …`, `a.HomeRelay = …`).

- [ ] **Step 1: Add a constructor for the Nostr verifier**

So the Nostr verifier's NIP-05 lookups use the SSRF-guarded client, add to
`internal/verify/nostr.go` (and add `"time"` to that file's import block):
```go
// NewNostrVerifier builds a Nostr verifier whose NIP-05 lookups use an
// SSRF-guarded client with the given timeout.
func NewNostrVerifier(timeout time.Duration) *NostrVerifier {
	return &NostrVerifier{HTTP: newSafeClient(timeout)}
}
```

- [ ] **Step 2: Wire the verifier into the API in main.go**

Add the import to `cmd/publisher/main.go`:
```go
	"github.com/geofox/publisher/internal/verify"
```

Add this block right after the existing `a.HomeRelay = cfg.NIP65BootstrapRelay` line:
```go
	a.Verify = &verify.Service{
		Nostr:    verify.NewNostrVerifier(cfg.VerifyHTTPTimeout),
		Bluesky:  verify.NewBlueskyVerifier(cfg.PLCDirectoryURL, cfg.VerifyHTTPTimeout),
		Mastodon: verify.NewMastodonVerifier(cfg.VerifyHTTPTimeout),
	}
```

- [ ] **Step 3: Build**

Run: `go build ./...`
Expected: builds clean.

- [ ] **Step 4: Smoke-test the endpoint**

Run (in one shell):
```bash
NSEC_HEX=0000000000000000000000000000000000000000000000000000000000000001 \
OWNER_PUBKEY=79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798 \
BLOSSOM_URL=https://blossom.example.com DB_PATH=/tmp/verify-smoke.db \
  go run ./cmd/publisher &
sleep 1
curl -s -X POST localhost:8080/api/verify -H 'content-type: application/json' \
  -d '{"input":"{\"kind\":1,\"id\":\"dc90c95f09947507c1044e8f48bcf6350aa6bff1507dd4acfc755b9239b5c962\",\"pubkey\":\"3bf0c63fcb93463407af97a5e5ee64fa883d107ef9e558472c4eb9aaaefa459d\",\"created_at\":1644271588,\"tags\":[],\"content\":\"now that https://blueskyweb.org/blog/2-7-2022-overview was announced we can stop working on nostr?\",\"sig\":\"230e9d8f0ddaf7eb70b5f7741ccfa37e87a455c9a469282e3464e2052d3192cd63a167e196e381ef9d7e69e9ea43af2443b839974dc85d8aaab9efe1d9296524\"}"}'
kill %1
```
Expected: JSON with `"status":"verified"`, `"platform":"nostr"`.

- [ ] **Step 5: Commit**

```bash
git add cmd/publisher/main.go internal/verify/nostr.go
git commit -m "publisher: wire verify.Service into the API"
```

---

## Task 10: Web UI — the "verify" tab

**Files:**
- Modify: `internal/web/assets/index.html`
- Create: `internal/web/assets/verify.js`
- Modify: `internal/web/assets/main.js`
- Modify: `internal/web/assets/app.css`
- Test: `internal/web/embed_test.go` (add an assertion the asset embeds)

- [ ] **Step 1: Add the tab button and section to `index.html`**

In the `<nav class="tabs">` block, add after the tools tab button:
```html
      <button class="tab" data-view="verify" type="button">verify</button>
```

Before `</main>`, add the section:
```html
    <!-- ── Verify ───────────────────────────────────────────────────────── -->
    <section id="verify" class="view" hidden>
      <div class="sec-head"><span class="lbl">verify a post</span><span class="muted">paste a Nostr event, or a Bluesky / Mastodon URL</span></div>
      <textarea id="vinput" class="master" placeholder="paste event JSON, or https://bsky.app/… / https://mastodon.…/…"></textarea>
      <div class="sec-head"><span class="lbl">expected identity</span><span class="muted">optional · npub / handle / @user@domain</span></div>
      <input id="vexpected" type="text" placeholder="optional — who do you think posted this?">
      <button id="vsubmit" class="submit" type="button">Verify</button>
      <div id="vresult"></div>
    </section><!-- /#verify -->
```

- [ ] **Step 2: Read `main.js` and a sibling module to match conventions**

Run: `cat internal/web/assets/main.js && echo '---' && sed -n '1,40p' internal/web/assets/tools.js`
Note how modules are imported/initialized and how `fetch` calls and tab switching work.

- [ ] **Step 3: Create `verify.js`**

```js
// verify.js — the "verify" tab: submit an event/URL to /api/verify and render
// the tri-state verdict.

const $ = (id) => document.getElementById(id);

function chip(status) {
  const map = {
    verified: ["✓ verified", "ok"],
    failed: ["✗ failed", "err"],
    error: ["⚠ could not verify", "warn"],
  };
  const [label, cls] = map[status] || ["?", "warn"];
  return `<span class="vchip ${cls}">${label}</span>`;
}

function renderVerdict(v) {
  if (!v || !v.status) return `<div class="vcard">no result</div>`;
  const rows = [];
  rows.push(`<div class="vhead">${chip(v.status)}`);
  if (v.assurance) rows.push(`<span class="vbadge">${v.assurance}</span>`);
  rows.push(`<span class="muted"> ${v.platform || ""}</span></div>`);
  if (v.error) rows.push(`<div class="verr">${escapeHTML(v.error)}</div>`);
  if (v.signer) rows.push(`<div class="vsigner">signer: ${escapeHTML(signerLabel(v.signer))}</div>`);
  if (v.expected) {
    const m = v.expected.matches ? "matches ✓" : "does NOT match ✗";
    rows.push(`<div class="vmatch">${m} expected “${escapeHTML(v.expected.provided)}”${v.expected.detail ? " — " + escapeHTML(v.expected.detail) : ""}</div>`);
  }
  if (v.content && v.content.text) rows.push(`<div class="vexcerpt">${escapeHTML(v.content.text)}</div>`);
  if (Array.isArray(v.warnings)) v.warnings.forEach((w) => rows.push(`<div class="vwarn">${escapeHTML(w)}</div>`));
  if (Array.isArray(v.checks) && v.checks.length) {
    const items = v.checks.map((c) => `<li class="vcheck ${c.result}">${escapeHTML(c.name)}: ${c.result}${c.detail ? ` — ${escapeHTML(c.detail)}` : ""}</li>`).join("");
    rows.push(`<details class="vchecks"><summary>checks</summary><ul>${items}</ul></details>`);
  }
  return `<div class="vcard">${rows.join("")}</div>`;
}

function signerLabel(s) {
  if (s.acct) return s.acct;
  if (s.handle || s.did) return `${s.handle || ""}${s.did ? " (" + s.did + ")" : ""}`;
  if (s.npub) return s.npub;
  return s.pubkey_hex || "(unknown)";
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
}

async function submitVerify() {
  const out = $("vresult");
  const input = $("vinput").value.trim();
  if (!input) { out.innerHTML = `<div class="vcard">enter an event or URL</div>`; return; }
  out.innerHTML = `<div class="vcard muted">verifying…</div>`;
  try {
    const resp = await fetch("/api/verify", {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ input, expected: $("vexpected").value.trim() }),
    });
    const v = await resp.json();
    out.innerHTML = renderVerdict(v);
  } catch (e) {
    out.innerHTML = `<div class="vcard verr">request failed: ${escapeHTML(e.message)}</div>`;
  }
}

export function initVerify() {
  const btn = $("vsubmit");
  if (btn) btn.addEventListener("click", submitVerify);
}
```

- [ ] **Step 4: Register `initVerify` in `main.js`**

Add the import and call following the existing module-init pattern in `main.js`. For example, if other modules are initialized like `initTools();`, add:
```js
import { initVerify } from "./verify.js";
// ... alongside the other init calls:
initVerify();
```
(Match the exact import/init style already in `main.js`.)

- [ ] **Step 5: Add CSS to `app.css`**

Append:
```css
/* verify tab */
.vcard { border: 1px solid var(--line, #222a3a); border-radius: 8px; padding: 12px; margin-top: 12px; }
.vhead { display: flex; align-items: center; gap: 8px; }
.vchip { font-weight: 600; padding: 2px 8px; border-radius: 6px; }
.vchip.ok { background: #0e2a18; color: #5fe39a; }
.vchip.err { background: #2a0e12; color: #ff7a8a; }
.vchip.warn { background: #2a230e; color: #ffd166; }
.vbadge { font-size: 0.8em; padding: 1px 6px; border: 1px solid var(--line, #222a3a); border-radius: 6px; color: var(--muted, #9aa4b2); }
.vsigner, .vmatch, .vexcerpt, .verr, .vwarn { margin-top: 8px; }
.vexcerpt { color: var(--muted, #9aa4b2); white-space: pre-wrap; }
.verr { color: #ff7a8a; }
.vwarn { color: #ffd166; }
.vchecks { margin-top: 8px; }
.vcheck.pass { color: #5fe39a; }
.vcheck.fail { color: #ff7a8a; }
.vcheck.skip { color: #9aa4b2; }
```
(Use the project's existing CSS variable names if they differ — check the top of `app.css`.)

- [ ] **Step 6: Add an embed assertion**

In `internal/web/embed_test.go`, follow the existing test pattern and add `verify.js` to whatever set of expected asset filenames the test checks (so a missing/renamed asset fails the build). If the test reads a specific list, add `"verify.js"` to it.

- [ ] **Step 7: Verify tab switching uses `data-view`**

The existing tab JS switches `.view` sections by `data-view`. The new button (`data-view="verify"`) and section (`id="verify"`) follow that convention, so no JS change is needed for switching. Confirm by reading the tab-switch code (in `common.js` or `main.js`).

- [ ] **Step 8: Build, test, manual check**

Run:
```bash
go test ./internal/web/ -v
go build ./cmd/publisher
```
Expected: PASS + clean build. Then run the server and open `http://localhost:8080`, click "verify", paste the known-good event JSON, click Verify → a green "✓ verified" card with a `cryptographic` badge.

- [ ] **Step 9: Commit**

```bash
git add internal/web/assets/index.html internal/web/assets/verify.js internal/web/assets/main.js internal/web/assets/app.css internal/web/embed_test.go
git commit -m "web: add verify tab UI"
```

---

## Task 11: Docs + full verification

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the endpoint and env vars**

In `README.md`, add a `### POST /api/verify` subsection under the HTTP API section:
```markdown
### `POST /api/verify`

Verify that a post is authentically signed by the claimed author. Read-only;
uses no credentials and never touches the signing key.

```json
{ "input": "<pasted Nostr event JSON | Bluesky/Mastodon post URL | at:// URI>",
  "platform": "",   // optional: nostr|bluesky|mastodon (else auto-detected)
  "expected": "" }  // optional: npub / handle / @user@domain to match the signer
```

Response is a verdict with a tri-state `status` (`verified` / `failed` /
`error`), an `assurance` level (`cryptographic` for Nostr, Bluesky, and
FEP-8b32-signed Mastodon posts; `origin` for plain Mastodon), the resolved
`signer`, an optional `expected` match, and a transparent `checks` list.

- **Nostr:** verifies the event id and Schnorr signature (offline).
- **Bluesky:** verifies the repo commit signature and the record's MST inclusion
  proof, fetched from the author's PDS.
- **Mastodon:** verifies origin authority (the actor's domain serves the object),
  plus a FEP-8b32 `eddsa-jcs-2022` integrity proof when present.

HTTP 200 for any completed verdict (including `failed`); 400 for malformed
input; 502 when the verifier itself could not complete (network/timeout).
```

Add the two new env vars to the configuration table:
```markdown
| `PLC_DIRECTORY_URL` | no | `https://plc.directory` | did:plc resolver for Bluesky identity lookups |
| `VERIFY_HTTP_TIMEOUT` | no | `10s` | Per-request timeout for verification fetches |
```

- [ ] **Step 2: Run the full test suite**

Run: `go test ./...`
Expected: PASS across all packages.

- [ ] **Step 3: Vet and build the static binary**

Run:
```bash
go vet ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/publisher.bin ./cmd/publisher
ls -la /tmp/publisher.bin
```
Expected: vet clean; binary builds (expect ~18–22 MB per the spec's dependency analysis).

- [ ] **Step 4: Run the Bluesky integration test once against a real post**

Run:
```bash
VERIFY_BSKY_URL="https://bsky.app/profile/<handle>/post/<rkey>" \
  go test ./internal/verify/ -run TestBlueskyIntegration -v
```
Expected: PASS — confirms the full commit-signature + MST-inclusion path works against live infrastructure.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document /api/verify and verification env vars"
rm -f /tmp/publisher.bin
```

---

## Self-Review notes (for the implementer)

- **Spec coverage:** Nostr offline verify (Task 3), Bluesky commit+MST (Task 5),
  Mastodon origin+FEP-8b32 JCS (Task 6), tri-state Verdict (Task 1), dispatcher +
  auto-detect + nevent rejection (Task 4), SSRF guard (Task 2), `/api/verify`
  (Task 7), config (Task 8), wiring (Task 9), web tab (Task 10), docs (Task 11) —
  every spec section maps to a task.
- **Known library-version checks:** three steps explicitly verify an API name
  against the pinned versions before relying on it — `nip19.EncodeNpub`
  (Task 3 Step 4), indigo `Repo` record methods (Task 5 Step 6). If a name
  differs, adjust the single call site noted there.
- **Test gating:** Bluesky and Nostr-NIP05 network paths are env-gated/skipped so
  `go test ./...` is hermetic; the live Bluesky path is exercised once in Task 11.
- **FEP-8b32 fixtures:** Task 6 tests the origin path and proof plumbing; a real
  signed fixture is scarce (see spec §11). If you obtain a `eddsa-jcs-2022`
  signed object, add it as a fixture asserting `assurance:"cryptographic"`.
```

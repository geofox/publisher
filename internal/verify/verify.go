// Package verify provides credential-free verification of social-media posts:
// it reports who actually signed a Nostr event or a Bluesky/Mastodon post, with
// an explicit assurance level, and optionally matches that signer against an
// expected identity. It never accesses the owner's signing key or credentials.
package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

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
	// Checks is always present in JSON. Producers MUST set it to a non-nil
	// slice (use []Check{} for "no sub-checks") so it serializes as [] not null,
	// which SPA/consumers range over without a nil guard.
	Checks   []Check  `json:"checks"`
	Warnings []string `json:"warnings,omitempty"`
	Error    string   `json:"error,omitempty"` // populated only when Status=="error"
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

// errVerdict builds a StatusError verdict (the check could not complete — not a
// forgery signal). Used by the platform verifiers (nostr.go, bluesky.go,
// mastodon.go) and the dispatcher. Checks is initialized non-nil per the
// Verdict.Checks contract.
func errVerdict(platform, msg string) Verdict {
	return Verdict{Platform: platform, Status: StatusError, Checks: []Check{}, Error: msg}
}

// Service routes an Input to the right platform verifier.
type Service struct {
	Nostr    Verifier
	Bluesky  Verifier
	Mastodon Verifier
	Threads  Verifier
}

// Verify selects a verifier (explicit Platform override wins, else auto-detect)
// and runs it. Detection or routing failures return a StatusError verdict.
// A panic inside any verifier is recovered and returned as a StatusError so it
// cannot crash the calling handler (spec §9).
func (s *Service) Verify(ctx context.Context, in Input) (verdict Verdict) {
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
	defer func() {
		if r := recover(); r != nil {
			verdict = errVerdict(platform, fmt.Sprintf("internal verifier error: %v", r))
		}
	}()
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
	case "threads":
		return s.Threads
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
		if isThreadsHost(host) {
			return "threads", nil
		}
		return "mastodon", nil
	default:
		return "", fmt.Errorf("unrecognized input: paste an event JSON or a post URL")
	}
}

// isThreadsHost reports whether host is a Threads (Meta) web domain. Threads
// serves on both threads.com (current canonical) and the older threads.net.
func isThreadsHost(host string) bool {
	for _, d := range []string{"threads.com", "threads.net"} {
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

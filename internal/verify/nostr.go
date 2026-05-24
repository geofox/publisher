package verify

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip05"
	"fiatjaf.com/nostr/nip19"
)

// NostrVerifier verifies a pasted Nostr event. Verification is fully offline;
// HTTP is used only to resolve an expected NIP-05 identity, when supplied.
type NostrVerifier struct {
	HTTP *http.Client // SSRF-guarded; used only for NIP-05 resolution
}

// NewNostrVerifier builds a Nostr verifier whose NIP-05 lookups use an
// SSRF-guarded client with the given timeout.
func NewNostrVerifier(timeout time.Duration) *NostrVerifier {
	return &NostrVerifier{HTTP: newSafeClient(timeout)}
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
		v.Checks = append(v.Checks, check("signature_valid", sigOK,
			"schnorr signature is valid for the event pubkey"))
	} else {
		v.Checks = append(v.Checks, Check{Name: "signature_valid", Result: "skip",
			Detail: "not checked: event id mismatch"})
	}

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
		pk, ok := val.(nostr.PubKey)
		if !ok {
			return nostr.PubKey{}, fmt.Errorf("npub did not decode to a pubkey")
		}
		return pk, nil
	case len(exp) == 64 && isHex(exp):
		b, err := hex.DecodeString(exp)
		if err != nil {
			return nostr.PubKey{}, err
		}
		return nostr.PubKey(b), nil
	case strings.Contains(exp, "@"):
		return nv.resolveNIP05(ctx, exp)
	default:
		return nostr.PubKey{}, fmt.Errorf("unrecognized identity format")
	}
}

// resolveNIP05 resolves a NIP-05 "name@domain" identifier to a pubkey by
// fetching the domain's /.well-known/nostr.json through the SSRF-guarded client.
// We do the fetch ourselves (rather than nip05.QueryIdentifier) so it uses
// nv.HTTP and the SSRF guard applies to the user-supplied domain.
func (nv *NostrVerifier) resolveNIP05(ctx context.Context, address string) (nostr.PubKey, error) {
	name, _, err := nip05.ParseIdentifier(address)
	if err != nil {
		return nostr.PubKey{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nip05.IdentifierToURL(address), nil)
	if err != nil {
		return nostr.PubKey{}, err
	}
	resp, err := nv.HTTP.Do(req)
	if err != nil {
		return nostr.PubKey{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nostr.PubKey{}, fmt.Errorf("nip05 fetch status %d", resp.StatusCode)
	}
	var wk nip05.WellKnownResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&wk); err != nil {
		return nostr.PubKey{}, err
	}
	pk, ok := wk.Names[name]
	if !ok {
		return nostr.PubKey{}, fmt.Errorf("no nip05 entry for %q", name)
	}
	return pk, nil
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

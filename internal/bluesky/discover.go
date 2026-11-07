package bluesky

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/geofox/publisher/internal/destination"
)

// Discovery finds the PDS an account lives on from its handle or DID, as
// the atproto specs describe (atproto.com/specs/handle, /specs/did):
//
//   - a handle's DID from DNS (a TXT record at _atproto.<handle> holding
//     did=<DID>), else from https://<handle>/.well-known/atproto-did;
//   - the DID document from the PLC directory (did:plc) or from
//     https://<host>/.well-known/did.json (did:web, a host name only);
//   - the first service whose id ends in #atproto_pds and whose type is
//     AtprotoPersonalDataServer.
//
// A handle counts only when the DID document claims it back (its first
// at:// handle in alsoKnownAs), so a handle can't send its owner's
// password to a server another account names. An email can't be looked
// up.
type Discovery struct {
	// HTTP reaches the handle's host and a did:web host: addresses that
	// come from what the owner typed, so the guard's.
	HTTP *http.Client
	// PLC reaches PLCURL, the operator's directory.
	PLC    *http.Client
	PLCURL string
	// LookupTXT looks up DNS TXT records (nil: the system resolver).
	LookupTXT func(ctx context.Context, name string) ([]string, error)
}

// Discovery's answers, for the owner. None names an internal address.
var (
	errNotHandle   = errors.New("enter a handle (you.bsky.social), a DID (did:plc:…) or the account's email")
	errNoHandleDID = errors.New("this handle doesn't lead to an account: check it, or enter the PDS address under Advanced")
	errAmbiguous   = errors.New("this handle's DNS names more than one account (_atproto TXT records)")
	errNotClaimed  = errors.New("the account this handle leads to doesn't claim the handle back, so it isn't used: check the handle, or use your DID")
	errNoDocument  = errors.New("the account's DID document couldn't be read: try again, or enter the PDS address under Advanced")
	errNoPDS       = errors.New("the account's DID document names no PDS Publisher can use: enter the PDS address under Advanced")
	errDIDMethod   = errors.New("only did:plc and did:web accounts can be looked up: enter the PDS address under Advanced")
	errEmail       = errors.New("an email can't be looked up: enter the PDS address under Advanced, or use your handle")
)

// systemLookupTXT is the system resolver's (tests replace it).
var systemLookupTXT = net.DefaultResolver.LookupTXT

const (
	maxWellKnown   = 2 << 10  // a DID, with room for stray whitespace
	maxDIDDocument = 64 << 10 // real documents are about 1 KiB
)

// Identifier is what the owner typed, read: a handle (lowercase, a leading
// @ dropped), a DID, or an email.
type Identifier struct {
	Handle syntax.Handle
	DID    syntax.DID
	Email  string
}

// ParseIdentifier reads a Bluesky identifier.
func ParseIdentifier(raw string) (Identifier, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "did:") {
		did, err := syntax.ParseDID(s)
		if err != nil {
			return Identifier{}, errNotHandle
		}
		return Identifier{DID: did}, nil
	}
	if h, err := syntax.ParseHandle(strings.TrimPrefix(s, "@")); err == nil {
		return Identifier{Handle: h.Normalize()}, nil
	}
	if at := strings.LastIndexByte(s, '@'); at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n") {
		return Identifier{Email: s}, nil
	}
	return Identifier{}, errNotHandle
}

// String is the identifier as createSession takes it.
func (id Identifier) String() string {
	switch {
	case id.Handle != "":
		return id.Handle.String()
	case id.DID != "":
		return id.DID.String()
	}
	return id.Email
}

// PDS finds the PDS of a handle or DID. Its errors are for the owner.
func (d Discovery) PDS(ctx context.Context, id Identifier) (string, error) {
	did := id.DID
	if id.Handle != "" {
		if !id.Handle.AllowedTLD() {
			return "", errNoHandleDID
		}
		var err error
		if did, err = d.handleDID(ctx, id.Handle); err != nil {
			return "", err
		}
	}
	if did == "" {
		return "", errEmail
	}
	doc, err := d.document(ctx, did)
	if err != nil {
		return "", err
	}
	if id.Handle != "" && doc.claimedHandle() != id.Handle {
		return "", errNotClaimed
	}
	return doc.pds(did)
}

// handleDID resolves a handle: DNS first (the spec prefers it when the
// two disagree), then the well-known address.
func (d Discovery) handleDID(ctx context.Context, h syntax.Handle) (syntax.DID, error) {
	did, err := d.dnsDID(ctx, h)
	if err == nil || errors.Is(err, errAmbiguous) {
		return did, err
	}
	return d.wellKnownDID(ctx, h)
}

func (d Discovery) dnsDID(ctx context.Context, h syntax.Handle) (syntax.DID, error) {
	lookup := d.LookupTXT
	if lookup == nil {
		lookup = systemLookupTXT
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	records, err := lookup(cctx, "_atproto."+h.String())
	if err != nil {
		return "", errNoHandleDID
	}
	var found syntax.DID
	for _, r := range records {
		v, ok := strings.CutPrefix(strings.TrimSpace(r), "did=")
		if !ok {
			continue // other TXT records are ignored
		}
		did, err := syntax.ParseDID(v)
		switch {
		case err != nil:
			continue
		case found != "" && did != found:
			return "", errAmbiguous
		}
		found = did
	}
	if found == "" {
		return "", errNoHandleDID
	}
	return found, nil
}

func (d Discovery) wellKnownDID(ctx context.Context, h syntax.Handle) (syntax.DID, error) {
	body, err := get(ctx, d.HTTP, "https://"+h.String()+"/.well-known/atproto-did", maxWellKnown)
	if err != nil {
		return "", errNoHandleDID
	}
	did, err := syntax.ParseDID(strings.TrimSpace(string(body)))
	if err != nil {
		return "", errNoHandleDID
	}
	return did, nil
}

// didDocument is the part of a DID document discovery reads.
type didDocument struct {
	ID          string   `json:"id"`
	AlsoKnownAs []string `json:"alsoKnownAs"`
	Service     []struct {
		ID              string          `json:"id"`
		Type            string          `json:"type"`
		ServiceEndpoint json.RawMessage `json:"serviceEndpoint"`
	} `json:"service"`
}

func (d Discovery) document(ctx context.Context, did syntax.DID) (didDocument, error) {
	var client *http.Client
	var docURL string
	switch did.Method() {
	case "plc":
		client, docURL = d.PLC, strings.TrimRight(d.PLCURL, "/")+"/"+did.String()
	case "web":
		// A host name only: no port, no path (the spec's restriction).
		host, err := syntax.ParseHandle(did.Identifier())
		if err != nil || !host.AllowedTLD() {
			return didDocument{}, errDIDMethod
		}
		client, docURL = d.HTTP, "https://"+host.Normalize().String()+"/.well-known/did.json"
	default:
		return didDocument{}, errDIDMethod
	}
	body, err := get(ctx, client, docURL, maxDIDDocument)
	if err != nil {
		return didDocument{}, errNoDocument
	}
	var doc didDocument
	if err := json.Unmarshal(body, &doc); err != nil || doc.ID != did.String() {
		return didDocument{}, errNoDocument
	}
	return doc, nil
}

// claimedHandle is the first valid handle in alsoKnownAs (the one the
// account claims, whether or not it resolves back).
func (doc didDocument) claimedHandle() syntax.Handle {
	for _, u := range doc.AlsoKnownAs {
		rest, ok := strings.CutPrefix(u, "at://")
		if !ok {
			continue
		}
		if h, err := syntax.ParseHandle(rest); err == nil {
			return h.Normalize()
		}
	}
	return ""
}

// pds is the first #atproto_pds service's address: a URL of a scheme,
// host and optional port only.
func (doc didDocument) pds(did syntax.DID) (string, error) {
	for _, s := range doc.Service {
		if (s.ID != "#atproto_pds" && s.ID != did.String()+"#atproto_pds") || s.Type != "AtprotoPersonalDataServer" {
			continue
		}
		var endpoint string
		if err := json.Unmarshal(s.ServiceEndpoint, &endpoint); err != nil {
			return "", errNoPDS
		}
		u, err := url.Parse(endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return "", errNoPDS
		}
		return u.Scheme + "://" + u.Host, nil
	}
	return "", errNoPDS
}

// get reads a small document: a 2xx answer, at most limit bytes.
func get(ctx context.Context, client *http.Client, u string, limit int64) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("too large")
	}
	return b, nil
}

var _ destination.ConfigDiscoverer = Driver{}

// DiscoverConfig implements destination.ConfigDiscoverer: the identifier
// as createSession takes it (a handle in lowercase, without a leading @)
// and, when no PDS address was given, the one found from the handle or
// DID.
func (Driver) DiscoverConfig(ctx context.Context, req destination.DiscoverRequest) (map[string]any, error) {
	raw, _ := destination.ConfigString(req.Config, ConfigIdentifier)
	id, err := ParseIdentifier(raw)
	if err != nil {
		return nil, &destination.FieldError{Key: ConfigIdentifier, Code: "invalid", Detail: err.Error()}
	}
	out := map[string]any{ConfigIdentifier: id.String()}
	if pds, _ := destination.ConfigString(req.Config, ConfigPDSURL); pds != "" {
		return out, nil
	}
	if id.Email != "" {
		return nil, &destination.FieldError{Key: ConfigPDSURL, Code: "required", Detail: errEmail.Error()}
	}
	plc := req.Tuning.PLCDirectoryURL
	if plc == "" {
		plc = "https://plc.directory"
	}
	pds, err := Discovery{HTTP: req.Net.Client(10 * time.Second), PLC: destination.TrustedNet().Client(10 * time.Second), PLCURL: plc}.PDS(ctx, id)
	if err != nil {
		return nil, &destination.FieldError{Key: ConfigIdentifier, Code: "not_found", Detail: err.Error()}
	}
	out[ConfigPDSURL] = pds
	return out, nil
}

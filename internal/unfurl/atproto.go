package unfurl

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// didRe constrains DIDs embedded in at:// URIs to RFC-ish DID syntax — in
// particular no '/', '?' or '#', which would smuggle path/query segments
// into the PLC directory or did:web URL we build from it.
var didRe = regexp.MustCompile(`^did:[a-z0-9]+:[A-Za-z0-9._%:-]+$`)

// resolveRef resolves an at://did/collection/rkey URI to a StrongRef: DID
// document → owner's PDS → unauthenticated com.atproto.repo.getRecord for the
// record's CID. Handle-based at:// URIs are rejected (site.standard tags are
// specified to carry DIDs).
func (s *Service) resolveRef(ctx context.Context, atURI string) (StrongRef, error) {
	rest := strings.TrimPrefix(atURI, "at://")
	if rest == atURI {
		return StrongRef{}, fmt.Errorf("not an at:// uri: %q", atURI)
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return StrongRef{}, fmt.Errorf("malformed at:// uri: %q", atURI)
	}
	did, collection, rkey := parts[0], parts[1], parts[2]
	if !didRe.MatchString(did) {
		return StrongRef{}, fmt.Errorf("at:// authority is not a valid DID: %q", did)
	}
	pds, err := s.resolvePDS(ctx, did)
	if err != nil {
		return StrongRef{}, err
	}
	q := url.Values{"repo": {did}, "collection": {collection}, "rkey": {rkey}}
	var rec struct {
		URI string `json:"uri"`
		CID string `json:"cid"`
	}
	if err := s.getJSON(ctx, pds+"/xrpc/com.atproto.repo.getRecord?"+q.Encode(), &rec); err != nil {
		return StrongRef{}, fmt.Errorf("getRecord %s: %w", atURI, err)
	}
	if rec.CID == "" {
		return StrongRef{}, fmt.Errorf("getRecord %s: empty cid", atURI)
	}
	return StrongRef{URI: rec.URI, CID: rec.CID}, nil
}

// didWebDocURL maps a did:web identifier to its DID document URL: bare
// domains use /.well-known/did.json; path-form DIDs (colon-separated path
// segments) use https://<domain>/<path>/did.json.
func didWebDocURL(did string) string {
	rest := strings.TrimPrefix(did, "did:web:")
	parts := strings.Split(rest, ":")
	if len(parts) == 1 {
		return "https://" + parts[0] + "/.well-known/did.json"
	}
	return "https://" + strings.Join(parts, "/") + "/did.json"
}

// resolvePDS returns the PDS base URL from the DID document's atproto_pds
// service entry. Supports did:plc (via the PLC directory) and did:web.
func (s *Service) resolvePDS(ctx context.Context, did string) (string, error) {
	var docURL string
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		docURL = s.PLCDirectory + "/" + did
	case strings.HasPrefix(did, "did:web:"):
		docURL = didWebDocURL(did)
	default:
		return "", fmt.Errorf("unsupported DID method: %s", did)
	}
	var doc struct {
		Service []struct {
			ID              string `json:"id"`
			Type            string `json:"type"`
			ServiceEndpoint string `json:"serviceEndpoint"`
		} `json:"service"`
	}
	if err := s.getJSON(ctx, docURL, &doc); err != nil {
		return "", fmt.Errorf("resolve %s: %w", did, err)
	}
	for _, svc := range doc.Service {
		if svc.ServiceEndpoint == "" {
			continue
		}
		if svc.Type == "AtprotoPersonalDataServer" || strings.HasSuffix(svc.ID, "#atproto_pds") {
			return strings.TrimRight(svc.ServiceEndpoint, "/"), nil
		}
	}
	return "", fmt.Errorf("no atproto_pds service in DID document for %s", did)
}

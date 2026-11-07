package bluesky

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/netguard"
)

// atproto is a fake of the handle hosts, did:web hosts and the PLC
// directory: one TLS server answering by Host, and a plain one for PLC.
type atproto struct {
	mu        sync.Mutex
	wellKnown map[string]string // host → body of /.well-known/atproto-did
	didWeb    map[string]string // host → /.well-known/did.json
	plc       map[string]string // did → document
	txt       map[string][]string
	status    map[string]int // host → status other than 200
	hosts     []string       // hosts asked, in order
	web, dir  *httptest.Server
}

func newAtproto(t *testing.T) *atproto {
	a := &atproto{wellKnown: map[string]string{}, didWeb: map[string]string{}, plc: map[string]string{}, txt: map[string][]string{}, status: map[string]int{}}
	a.web = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.hosts = append(a.hosts, r.Host)
		var body string
		var ok bool
		switch r.URL.Path {
		case "/.well-known/atproto-did":
			body, ok = a.wellKnown[r.Host]
		case "/.well-known/did.json":
			body, ok = a.didWeb[r.Host]
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		if code := a.status[r.Host]; code != 0 {
			w.WriteHeader(code)
		}
		_, _ = w.Write([]byte(body))
	}))
	a.dir = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		body, ok := a.plc[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(a.web.Close)
	t.Cleanup(a.dir.Close)
	return a
}

// discovery reaches every host at the fake (its certificate is for
// example.com only, so names aren't checked).
func (a *atproto) discovery() Discovery {
	addr := a.web.Listener.Addr().String()
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test's own server
	}
	return Discovery{HTTP: &http.Client{Transport: tr}, PLC: a.dir.Client(), PLCURL: a.dir.URL + "/",
		LookupTXT: func(_ context.Context, name string) ([]string, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			if r, ok := a.txt[name]; ok {
				return r, nil
			}
			return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
		}}
}

func doc(did, handle, pds string) string {
	return fmt.Sprintf(`{"id":%q,"alsoKnownAs":["at://%s"],"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":%q}]}`,
		did, handle, pds)
}

func pdsOf(t *testing.T, d Discovery, raw string) (string, error) {
	t.Helper()
	id, err := ParseIdentifier(raw)
	if err != nil {
		return "", err
	}
	return d.PDS(context.Background(), id)
}

func TestParseIdentifier(t *testing.T) {
	for raw, want := range map[string]Identifier{
		"Alice.Example.COM":       {Handle: "alice.example.com"},
		" @alice.example.com ":    {Handle: "alice.example.com"},
		"did:plc:abc234":          {DID: "did:plc:abc234"},
		"did:web:alice.example":   {DID: "did:web:alice.example"},
		"alice@mail.example.com":  {Email: "alice@mail.example.com"},
		"alice+x@mail.example.co": {Email: "alice+x@mail.example.co"},
	} {
		if got, err := ParseIdentifier(raw); err != nil || got != want {
			t.Errorf("%q: %+v %v", raw, got, err)
		}
	}
	for _, raw := range []string{"", "alice", "did:plc:", "did:plc:a/b", "@", "a@", "@b", "a b@c.d", "localhost"} {
		if got, err := ParseIdentifier(raw); err == nil {
			t.Errorf("%q read as %+v", raw, got)
		}
	}
}

// A handle leads to its DID by DNS or else the well-known address, its
// DID document must claim it back, and the PDS is the document's.
func TestDiscoverFromHandle(t *testing.T) {
	a := newAtproto(t)
	d := a.discovery()
	a.txt["_atproto.dns.example.com"] = []string{"v=spf1 -all", "did=not-a-did", "did=did:plc:dnsuser"}
	a.plc["did:plc:dnsuser"] = doc("did:plc:dnsuser", "DNS.example.com", "https://pds.example.net")
	a.wellKnown["web.example.com"] = "  did:plc:webuser\n"
	a.plc["did:plc:webuser"] = doc("did:plc:webuser", "web.example.com", "https://pds2.example.net:8443/")
	for raw, want := range map[string]string{
		"@DNS.example.com": "https://pds.example.net",
		"web.example.com":  "https://pds2.example.net:8443",
	} {
		if got, err := pdsOf(t, d, raw); err != nil || got != want {
			t.Errorf("%s: %q %v", raw, got, err)
		}
	}
	// DNS wins over the well-known address.
	a.wellKnown["dns.example.com"] = "did:plc:someoneelse"
	if got, err := pdsOf(t, d, "dns.example.com"); err != nil || got != "https://pds.example.net" {
		t.Errorf("DNS first: %q %v", got, err)
	}
	// Two accounts in DNS: refused, without asking the well-known address.
	a.txt["_atproto.two.example.com"] = []string{"did=did:plc:one", "did=did:plc:two"}
	a.wellKnown["two.example.com"] = "did:plc:webuser"
	if _, err := pdsOf(t, d, "two.example.com"); !errors.Is(err, errAmbiguous) {
		t.Errorf("two DNS records: %v", err)
	}
	// The same DID twice is one account.
	a.txt["_atproto.twice.example.com"] = []string{"did=did:plc:dnsuser", "did=did:plc:dnsuser"}
	if _, err := pdsOf(t, d, "twice.example.com"); !errors.Is(err, errNotClaimed) {
		t.Errorf("one DID twice (whose document claims another handle): %v", err)
	}
	// An answer that isn't a success isn't read.
	a.wellKnown["broken.example.com"] = "did:plc:webuser"
	a.status["broken.example.com"] = http.StatusInternalServerError
	if _, err := pdsOf(t, d, "broken.example.com"); !errors.Is(err, errNoHandleDID) {
		t.Errorf("a 500 was read: %v", err)
	}
	// Nothing anywhere; a reserved name isn't asked at all.
	if _, err := pdsOf(t, d, "nobody.example.com"); !errors.Is(err, errNoHandleDID) {
		t.Errorf("unknown handle: %v", err)
	}
	a.mu.Lock()
	a.hosts = nil
	a.mu.Unlock()
	if _, err := pdsOf(t, d, "alice.local"); !errors.Is(err, errNoHandleDID) {
		t.Errorf("reserved TLD: %v", err)
	}
	if len(a.hosts) != 0 {
		t.Errorf("a reserved name was asked: %v", a.hosts)
	}
}

// The DID document must claim the handle: its first valid at:// handle,
// in any case.
func TestDiscoverChecksTheHandleBack(t *testing.T) {
	a := newAtproto(t)
	d := a.discovery()
	a.wellKnown["mallory.example.com"] = "did:plc:victim"
	a.plc["did:plc:victim"] = doc("did:plc:victim", "victim.example.com", "https://pds.example.net")
	if _, err := pdsOf(t, d, "mallory.example.com"); !errors.Is(err, errNotClaimed) {
		t.Errorf("a handle the document doesn't claim: %v", err)
	}
	a.wellKnown["second.example.com"] = "did:plc:two"
	a.plc["did:plc:two"] = `{"id":"did:plc:two","alsoKnownAs":["https://x.example.com","at://not a handle","at://first.example.com","at://second.example.com"],` +
		`"service":[{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.net"}]}`
	if _, err := pdsOf(t, d, "second.example.com"); !errors.Is(err, errNotClaimed) {
		t.Errorf("only the first valid handle counts: %v", err)
	}
	if got, err := pdsOf(t, d, "first.example.com"); err == nil {
		t.Errorf("first.example.com doesn't lead to the DID, yet: %q", got)
	}
	a.wellKnown["first.example.com"] = "did:plc:two"
	if got, err := pdsOf(t, d, "FIRST.example.com"); err != nil || got != "https://pds.example.net" {
		t.Errorf("first handle: %q %v", got, err)
	}
	// A DID needs no handle.
	if got, err := pdsOf(t, d, "did:plc:victim"); err != nil || got != "https://pds.example.net" {
		t.Errorf("DID: %q %v", got, err)
	}
}

// did:web: a host name only, its document from the host.
func TestDiscoverDIDWeb(t *testing.T) {
	a := newAtproto(t)
	d := a.discovery()
	a.didWeb["alice.example.com"] = doc("did:web:alice.example.com", "alice.example.com", "https://pds.example.net")
	if got, err := pdsOf(t, d, "did:web:alice.example.com"); err != nil || got != "https://pds.example.net" {
		t.Errorf("did:web: %q %v", got, err)
	}
	for _, did := range []string{"did:web:localhost%3A8080", "did:web:alice.example.com:user", "did:web:alice.local", "did:key:z6Mk", "did:example:123"} {
		if _, err := pdsOf(t, d, did); !errors.Is(err, errDIDMethod) {
			t.Errorf("%s: %v", did, err)
		}
	}
}

func TestDiscoverReadsTheDocumentCarefully(t *testing.T) {
	a := newAtproto(t)
	d := a.discovery()
	set := func(body string) { a.plc["did:plc:x"] = body }
	svc := func(services string) string { return `{"id":"did:plc:x","service":[` + services + `]}` }
	for name, c := range map[string]struct {
		body string
		want string
		err  error
	}{
		"another DID's document": {doc("did:plc:y", "x.example.com", "https://pds.example.net"), "", errNoDocument},
		"not JSON":               {"<html>", "", errNoDocument},
		"too large":              {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.net"}`) + strings.Repeat(" ", maxDIDDocument), "", errNoDocument},
		"no service":             {svc(``), "", errNoPDS},
		"full id": {svc(`{"id":"did:plc:x#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://full.example.net"}`),
			"https://full.example.net", nil},
		"another DID's id": {svc(`{"id":"did:plc:y#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://y.example.net"}`), "", errNoPDS},
		"wrong type, then right": {svc(`{"id":"#atproto_pds","type":"Other","serviceEndpoint":"https://other.example.net"},` +
			`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://right.example.net"},` +
			`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://later.example.net"}`), "https://right.example.net", nil},
		"a path":       {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.net/x"}`), "", errNoPDS},
		"user info":    {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://u:p@pds.example.net"}`), "", errNoPDS},
		"a query":      {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"https://pds.example.net?x"}`), "", errNoPDS},
		"other scheme": {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":"ftp://pds.example.net"}`), "", errNoPDS},
		"an object":    {svc(`{"id":"#atproto_pds","type":"AtprotoPersonalDataServer","serviceEndpoint":{"uri":"https://pds.example.net"}}`), "", errNoPDS},
	} {
		set(c.body)
		got, err := pdsOf(t, d, "did:plc:x")
		if got != c.want || !errors.Is(err, c.err) {
			t.Errorf("%s: %q %v", name, got, err)
		}
	}
}

// An email can't be looked up.
func TestDiscoverEmail(t *testing.T) {
	if _, err := pdsOf(t, newAtproto(t).discovery(), "alice@mail.example.com"); !errors.Is(err, errEmail) {
		t.Errorf("email: %v", err)
	}
}

// The driver's discovery: the identifier as createSession takes it, the
// PDS found only when none was given, its errors on the field to fix, the
// handle's host reached through the account's network, and a PDS on a
// private address refused by the account API's check.
func TestDiscoverConfig(t *testing.T) {
	a := newAtproto(t)
	old := systemLookupTXT
	systemLookupTXT = a.discovery().LookupTXT
	t.Cleanup(func() { systemLookupTXT = old })
	var dialed []string
	var mu sync.Mutex
	refusing := destination.Net{DialContext: func(_ context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return nil, errors.New("refused")
	}}
	discover := func(config map[string]any) (map[string]any, error) {
		return Driver{}.DiscoverConfig(context.Background(), destination.DiscoverRequest{Method: MethodAppPassword, Config: config,
			Net: refusing, Tuning: destination.Tuning{PLCDirectoryURL: a.dir.URL}})
	}
	fieldErr := func(err error) *destination.FieldError {
		var fe *destination.FieldError
		if !errors.As(err, &fe) {
			t.Fatalf("not a field error: %v", err)
		}
		return fe
	}
	// Given a PDS: no lookup, the identifier tidied.
	got, err := discover(map[string]any{"identifier": "@Alice.Example.com", "pds_url": "https://pds.example.net"})
	if err != nil || len(got) != 1 || got["identifier"] != "alice.example.com" {
		t.Errorf("with a PDS: %v %v", got, err)
	}
	if got, err := discover(map[string]any{"identifier": "alice@mail.example.com", "pds_url": "https://pds.example.net"}); err != nil || got["identifier"] != "alice@mail.example.com" {
		t.Errorf("email with a PDS: %v %v", got, err)
	}
	if fe := fieldErr(discoverErr(discover(map[string]any{"identifier": "alice@mail.example.com"}))); fe.Key != "pds_url" || fe.Code != "required" {
		t.Errorf("email: %+v", fe)
	}
	if fe := fieldErr(discoverErr(discover(map[string]any{"identifier": "not a handle"}))); fe.Key != "identifier" || fe.Code != "invalid" {
		t.Errorf("not a handle: %+v", fe)
	}
	// Found by DNS and the operator's PLC directory.
	a.txt["_atproto.alice.example.com"] = []string{"did=did:plc:alice"}
	a.plc["did:plc:alice"] = doc("did:plc:alice", "alice.example.com", "https://pds.example.net")
	if got, err := discover(map[string]any{"identifier": "Alice.example.com"}); err != nil || got["pds_url"] != "https://pds.example.net" || got["identifier"] != "alice.example.com" {
		t.Errorf("found: %v %v", got, err)
	}
	// The well-known address goes through the account's network.
	fe := fieldErr(discoverErr(discover(map[string]any{"identifier": "bob.example.com"})))
	if fe.Key != "identifier" || fe.Code != "not_found" || strings.Contains(fe.Detail, "refused") {
		t.Errorf("not found: %+v", fe)
	}
	mu.Lock()
	if len(dialed) != 1 || dialed[0] != "bob.example.com:443" {
		t.Errorf("dialed %v", dialed)
	}
	mu.Unlock()
	// A PDS on a private address is found, but the account API's check
	// refuses it.
	a.txt["_atproto.inside.example.com"] = []string{"did=did:plc:inside"}
	for _, pds := range []string{"https://127.0.0.1:8443", "https://10.0.0.5", "https://localhost"} {
		a.plc["did:plc:inside"] = doc("did:plc:inside", "inside.example.com", pds)
		got, err := discover(map[string]any{"identifier": "inside.example.com"})
		if err != nil {
			t.Fatalf("%s: %v", pds, err)
		}
		cfg, errs := destination.ValidateFields(Driver{}.Manifest().Config, map[string]any{"pds_url": got["pds_url"]},
			destination.FieldOptions{URL: func(raw string, r destination.URLRule) (string, error) {
				return netguard.Policy{}.OwnerURL(raw, netguard.URLOptions{Secure: r.Secure, Plain: r.Plain, Path: r.Path})
			}})
		if len(errs) != 1 || errs[0].Key != "pds_url" {
			t.Errorf("%s accepted: %v %+v", pds, cfg, errs)
		}
	}
}

func discoverErr(_ map[string]any, err error) error { return err }

package destination

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Config read from the environment (Go values) and from the database
// (JSON-decoded values) reads the same.
func TestConfigAccessors(t *testing.T) {
	goVals := map[string]any{"s": "x", "l": []string{"a", "b"}, "n": 16, "b": true}
	var jsonVals map[string]any
	raw, _ := json.Marshal(goVals)
	if err := json.Unmarshal(raw, &jsonVals); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]map[string]any{"go": goVals, "json": jsonVals} {
		if s, err := ConfigString(cfg, "s"); err != nil || s != "x" {
			t.Errorf("%s: ConfigString = %q, %v", name, s, err)
		}
		if l, err := ConfigStrings(cfg, "l"); err != nil || !reflect.DeepEqual(l, []string{"a", "b"}) {
			t.Errorf("%s: ConfigStrings = %v, %v", name, l, err)
		}
		if n, ok, err := ConfigInt(cfg, "n"); err != nil || !ok || n != 16 {
			t.Errorf("%s: ConfigInt = %d, %v, %v", name, n, ok, err)
		}
		if b, ok, err := ConfigBool(cfg, "b"); err != nil || !ok || !b {
			t.Errorf("%s: ConfigBool = %v, %v, %v", name, b, ok, err)
		}
		if _, ok, err := ConfigInt(cfg, "missing"); ok || err != nil {
			t.Errorf("%s: a missing int should be absent, not an error", name)
		}
	}
	if l, _ := ConfigStrings(map[string]any{"l": " a, ,b "}, "l"); !reflect.DeepEqual(l, []string{"a", "b"}) {
		t.Errorf("comma list = %v", l)
	}
	if _, _, err := ConfigInt(map[string]any{"n": 1.5}, "n"); err == nil {
		t.Error("1.5 accepted as a whole number")
	}
	if _, err := ConfigString(map[string]any{"s": 3.0}, "s"); err == nil {
		t.Error("a number accepted as a string")
	}
	if _, _, err := ConfigInt(map[string]any{"n": "16"}, "n"); err == nil {
		t.Error("text accepted as a number (ConfigBool refuses text too)")
	}
}

// The database decodes with UseNumber; numbers read the same from it.
func TestConfigJSONNumber(t *testing.T) {
	dec := json.NewDecoder(strings.NewReader(`{"n":28,"f":2.5}`))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if n, ok, err := ConfigInt(cfg, "n"); err != nil || !ok || n != 28 {
		t.Errorf("ConfigInt(json.Number) = %d, %v, %v", n, ok, err)
	}
	if _, _, err := ConfigInt(cfg, "f"); err == nil {
		t.Error("json.Number 2.5 accepted as a whole number")
	}
}

func TestConfigTime(t *testing.T) {
	at := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	raw, _ := json.Marshal(map[string]any{"t": at})
	var jsonVals map[string]any
	_ = json.Unmarshal(raw, &jsonVals)
	for name, cfg := range map[string]map[string]any{"go": {"t": at}, "json": jsonVals} {
		if got, ok, err := ConfigTime(cfg, "t"); err != nil || !ok || !got.Equal(at) {
			t.Errorf("%s: ConfigTime = %v, %v, %v", name, got, ok, err)
		}
	}
	if _, ok, err := ConfigTime(map[string]any{}, "t"); ok || err != nil {
		t.Error("a missing time should be absent, not an error")
	}
	if _, _, err := ConfigTime(map[string]any{"t": "tomorrow"}, "t"); err == nil {
		t.Error("tomorrow accepted as a time")
	}
}

// An accessor's error describes the problem without the key (InvalidField
// adds it once) and without the value (a secret in the wrong field must
// not reach a message).
func TestConfigErrorsNameNeitherKeyNorValue(t *testing.T) {
	cfg := map[string]any{"pds_url": 12345.0, "relays": []any{"wss://a", 67890.0}, "flag": "s3cr3t-value"}
	var errs []error
	_, err := ConfigString(cfg, "pds_url")
	errs = append(errs, err)
	_, err = ConfigStrings(cfg, "relays")
	errs = append(errs, err)
	_, _, err = ConfigBool(cfg, "flag")
	errs = append(errs, err)
	_, _, err = ConfigInt(cfg, "flag")
	errs = append(errs, err)
	for _, err := range errs {
		if err == nil {
			t.Fatal("a value of the wrong kind was accepted")
		}
		msg := InvalidField("field", err.Error()).Error()
		for _, leak := range []string{"pds_url", "relays", "12345", "67890", "s3cr3t"} {
			if strings.Contains(msg, leak) {
				t.Errorf("%q names %q", msg, leak)
			}
		}
		if strings.Count(msg, "field") != 1 {
			t.Errorf("%q should name the field once", msg)
		}
	}
}

// A state update is a patch: absent keys stay, "" or nil deletes.
func TestMergeState(t *testing.T) {
	secrets := map[string]string{"access_token": "old", "refresh_token": "r"}
	config := map[string]any{"user_id": "me", "expires_at": "2026-01-01T00:00:00Z"}
	s, c := MergeState(secrets, config, StateUpdate{
		Secrets: map[string]string{"access_token": "new", "refresh_token": ""},
		Config:  map[string]any{"expires_at": "2026-03-01T00:00:00Z", "gone": nil},
	})
	if !reflect.DeepEqual(s, map[string]string{"access_token": "new"}) {
		t.Errorf("secrets = %v", s)
	}
	if !reflect.DeepEqual(c, map[string]any{"user_id": "me", "expires_at": "2026-03-01T00:00:00Z"}) {
		t.Errorf("config = %v", c)
	}
	if secrets["access_token"] != "old" || config["expires_at"] != "2026-01-01T00:00:00Z" {
		t.Error("MergeState changed its inputs")
	}
}

// The contract types an out-of-process driver would exchange have fixed
// JSON names.
func TestContractJSONNames(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want string
	}{
		{CheckRequest{Authenticated: true}, `{"authenticated":true}`},
		{StateUpdate{Secrets: map[string]string{"t": "x"}, Config: map[string]any{"k": 1}}, `{"secrets":{"t":"x"},"config":{"k":1}}`},
		{EnvAccount{Key: "default", Label: "L", Method: "m", Config: map[string]any{"k": "v"}, Secrets: map[string]string{"s": "x"}},
			`{"key":"default","label":"L","method":"m","config":{"k":"v"},"secrets":{"s":"x"}}`},
		{EnvVar{Name: "X", Required: true}, `{"name":"X","required":true}`},
		{Tuning{PublishTimeout: time.Second, PLCDirectoryURL: "https://plc"},
			`{"publish_timeout":1000000000,"pow_timeout":0,"relay_cache_ttl":0,"verify_timeout":0,"plc_directory_url":"https://plc"}`},
		{Identity{RemoteID: "did:plc:x", Handle: "a"}, `{"remote_id":"did:plc:x","handle":"a"}`},
	} {
		raw, err := json.Marshal(tc.v)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != tc.want {
			t.Errorf("%T = %s, want %s", tc.v, raw, tc.want)
		}
	}
}

// The new contract types survive a JSON round trip, so an out-of-process
// driver can speak them.
func TestContractV2JSON(t *testing.T) {
	yes := true
	for _, v := range []any{
		Identity{RemoteID: "did:plc:x", Handle: "a.example"},
		Health{OK: true, Reachable: true, AuthOK: &yes, Message: "fine"},
		ResolvedSource{Ref: SourceRef{Type: "t", URI: "at://x"}, Preview: SourcePreview{Text: "hi",
			Media: []SourceMedia{{URL: "https://m", Video: true}}}, Actions: SourceActions{Reply: ActionAllowed{Allowed: true}}},
	} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		back := reflect.New(reflect.TypeOf(v))
		if err := json.Unmarshal(raw, back.Interface()); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(back.Elem().Interface(), v) {
			t.Errorf("round trip of %T: %+v", v, back.Elem().Interface())
		}
	}
}

// Clients built from a Net dial through it, so a host's policy reaches
// every connection a driver makes.
func TestNetDialsThroughPolicy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	var dials atomic.Int32
	base := TrustedNet()
	n := Net{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		dials.Add(1)
		return base.DialContext(ctx, network, addr)
	}}
	for _, c := range []*http.Client{n.Client(5 * time.Second), {Transport: n.Transport(TransportOptions{ResponseHeaderTimeout: time.Second})}} {
		resp, err := c.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
	}
	if dials.Load() != 2 {
		t.Fatalf("%d dials through the policy, want 2", dials.Load())
	}
}

func TestSafeRedirect(t *testing.T) {
	get := func(u string) *http.Request { r, _ := http.NewRequest(http.MethodGet, u, nil); return r }
	post := func(u string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(`{"password":"x"}`))
		return r
	}
	for _, tc := range []struct {
		name string
		req  *http.Request
		from string
		ok   bool
	}{
		{"same host", get("https://a.example/x"), "https://a.example/", true},
		{"other host, no body", get("https://b.example/x"), "https://a.example/", true},
		{"https to http", get("http://a.example/x"), "https://a.example/", false},
		{"body to another host", post("https://b.example/x"), "https://a.example/", false},
		{"body to the same host", post("https://a.example/y"), "https://a.example/", true},
		{"body to a subdomain", post("https://www.a.example/y"), "https://a.example/", true},
		{"body to the same host, other port", post("https://a.example:8443/y"), "https://a.example/", true},
		{"body to a parent domain", post("https://example/y"), "https://a.example/", false},
		{"body to a look-alike", post("https://evila.example/y"), "https://a.example/", false},
		{"body between IP addresses", post("https://10.0.0.1/y"), "https://0.0.1/", false},
	} {
		err := SafeRedirect(tc.req, []*http.Request{get(tc.from)})
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
	via := make([]*http.Request, 10)
	for i := range via {
		via[i] = get("https://a.example/")
	}
	if SafeRedirect(get("https://a.example/"), via) == nil {
		t.Error("an 11th redirect was followed")
	}
	// The body rule compares with the first request, not the last hop: a
	// chain can't walk the body away one subdomain at a time.
	chain := []*http.Request{get("https://a.example/"), get("https://b.a.example/")}
	if SafeRedirect(post("https://c.b.a.example/"), chain) != nil {
		t.Error("a subdomain of the first host was refused")
	}
	chain = []*http.Request{get("https://a.example/"), get("https://other.example/")}
	if SafeRedirect(post("https://x.other.example/"), chain) == nil {
		t.Error("a body followed a chain away from the first host")
	}
}

// A Net nobody filled in refuses to connect, instead of falling back to
// Go's default dialer and skipping the policy.
func TestZeroNetRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	resp, err := Net{}.Client(5 * time.Second).Get(srv.URL)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a zero Net connected")
	}
	if !strings.Contains(err.Error(), errNoNet.Error()) {
		t.Errorf("err = %v", err)
	}
}

// Transports speak HTTP/2 with a health check, so a dead connection is
// closed instead of holding every request to the server.
func TestTransportHTTP2HealthCheck(t *testing.T) {
	tr := TrustedNet().Transport(TransportOptions{})
	if !tr.ForceAttemptHTTP2 || tr.HTTP2 == nil || tr.HTTP2.SendPingTimeout <= 0 {
		t.Errorf("transport = HTTP/2 %v, config %+v", tr.ForceAttemptHTTP2, tr.HTTP2)
	}
}

// ContentNet refuses blocked addresses on the address dialed, and its
// error names no address.
func TestContentNet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer srv.Close()
	blockLoopback := func(ip net.IP) bool { return ip.IsLoopback() }
	_, err := ContentNet(blockLoopback).Client(5 * time.Second).Get(srv.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if strings.Contains(ErrBlockedAddress.Error(), "127.0.0.1") {
		t.Error("the error names the address")
	}
	resp, err := ContentNet(func(net.IP) bool { return false }).Client(5 * time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("an allowed address was refused: %v", err)
	}
	_ = resp.Body.Close()
	if n := ContentNet(blockLoopback); n.Proxy != nil {
		t.Error("ContentNet uses a proxy, which would dial past the check")
	}
}

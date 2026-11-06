package destination

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
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

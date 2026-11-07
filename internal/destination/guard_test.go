package destination

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func refuseLoopback(_ string, ip net.IP) error {
	if ip.IsLoopback() {
		return errors.New("loopback")
	}
	return nil
}

// The check is on the address connected to: a name that resolves to a
// refused address is refused at dial time, like an address literal.
func TestGuardedNetChecksTheConnectedAddress(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	c := GuardedNet(refuseLoopback).Client(0)
	for _, target := range []string{srv.URL, "http://localhost:" + u.Port()} {
		_, err := c.Get(target)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: %v, want ErrBlockedAddress", target, err)
		}
	}
	if hits.Load() != 0 {
		t.Error("the server was reached")
	}
	// The host name dialed reaches allow (for PRIVATE_DESTINATIONS names).
	var seen []string
	allow := func(host string, ip net.IP) error { seen = append(seen, host+"="+ip.String()); return nil }
	if _, err := GuardedNet(allow).Client(0).Get("http://localhost:" + u.Port()); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 || !strings.HasPrefix(seen[0], "localhost=") {
		t.Errorf("allow saw %v", seen)
	}
	// No proxy from the environment.
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:1")
	if GuardedNet(allow).Transport(TransportOptions{}).Proxy != nil {
		t.Error("guarded transport has a proxy")
	}
}

// A redirect is dialed through the guard too.
func TestGuardedNetRedirectToARefusedAddress(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer target.Close()
	tu, _ := url.Parse(target.URL)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://localhost:"+tu.Port()+"/", http.StatusFound)
	}))
	defer front.Close()
	// Only the address literal is allowed; the name "localhost" is not.
	allow := func(host string, ip net.IP) error {
		if host != "127.0.0.1" {
			return errors.New("no")
		}
		return nil
	}
	_, err := GuardedNet(allow).Client(0).Get(front.URL)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("redirect: %v", err)
	}
	if hits.Load() != 0 {
		t.Error("the redirect target was reached")
	}
}

func TestGuardedRedirectCredentials(t *testing.T) {
	var bHits atomic.Int32
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits.Add(1)
		if r.Header.Get("Authorization") != "" {
			t.Error("credential reached the other server")
		}
	}))
	defer b.Close()
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/same" {
			http.Redirect(w, r, "/ok", http.StatusFound)
			return
		}
		if r.URL.Path == "/ok" {
			return
		}
		http.Redirect(w, r, b.URL+"/", http.StatusTemporaryRedirect)
	}))
	defer a.Close()
	c := GuardedNet(func(string, net.IP) error { return nil }).Client(0)

	req, _ := http.NewRequest(http.MethodGet, a.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if _, err := c.Do(req); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Errorf("GET with Authorization to another origin: %v", err)
	}
	// Within its origin it follows.
	req, _ = http.NewRequest(http.MethodGet, a.URL+"/same", nil)
	req.Header.Set("Authorization", "Bearer secret")
	if resp, err := c.Do(req); err != nil || resp.Request.URL.Path != "/ok" {
		t.Errorf("same-origin redirect: %v", err)
	}
	// A POST is never redirected, even within its origin.
	if _, err := c.Post(a.URL+"/same", "application/json", strings.NewReader(`{"password":"x"}`)); err == nil || !strings.Contains(err.Error(), "POST") {
		t.Errorf("POST redirect: %v", err)
	}
	if bHits.Load() != 0 {
		t.Errorf("other server hit %d times", bHits.Load())
	}
	// Without a credential, a GET may change origin.
	if _, err := c.Get(a.URL + "/x"); err != nil || bHits.Load() != 1 {
		t.Errorf("plain GET: %v (hits %d)", err, bHits.Load())
	}
}

func TestGuardedRedirectLimitsAndDowngrade(t *testing.T) {
	mk := func(raw string) *http.Request { r, _ := http.NewRequest(http.MethodGet, raw, nil); return r }
	via := []*http.Request{mk("https://a.example/1")}
	if err := GuardedRedirect(mk("http://a.example/2"), via); err == nil {
		t.Error("https to http followed")
	}
	if err := GuardedRedirect(mk("https://a.example/2"), via); err != nil {
		t.Errorf("https to https: %v", err)
	}
	for i := 0; i < 4; i++ {
		via = append(via, mk("https://a.example/n"))
	}
	if err := GuardedRedirect(mk("https://a.example/6"), via); err == nil {
		t.Error("a sixth redirect followed")
	}
	if !sameOrigin(mk("https://A.example:443/x").URL, mk("https://a.example/y").URL) || sameOrigin(mk("https://a.example").URL, mk("https://a.example:8443").URL) ||
		sameOrigin(mk("https://a.example").URL, mk("http://a.example").URL) || sameOrigin(mk("https://a.example").URL, mk("https://b.a.example").URL) {
		t.Error("sameOrigin")
	}
}

package media

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

// blossomAuth decodes an upload's authorization event.
func blossomAuth(t *testing.T, r *http.Request) nostr.Event {
	t.Helper()
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Nostr ")
	if !ok {
		t.Fatalf("Authorization = %q", r.Header.Get("Authorization"))
	}
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ev nostr.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// With its own key, an upload's authorization names the server (BUD-11)
// and is signed by that key; with the environment's Nostr key it is as
// before, without the tag. Both send the hash they authorize.
func TestBlossomAuthorization(t *testing.T) {
	var got []nostr.Event
	var hashes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, blossomAuth(t, r))
		hashes = append(hashes, r.Header.Get("X-SHA-256"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	host := strings.ToLower(strings.Split(strings.TrimPrefix(srv.URL, "http://"), ":")[0])
	own, env := nostr.Generate(), nostr.Generate()
	ctx := context.Background()
	sha := strings.Repeat("ab", 32)
	for _, b := range []*Blossom{{URL: srv.URL, Key: own, Own: true, HTTP: srv.Client()}, {URL: srv.URL, Key: env, HTTP: srv.Client()}} {
		u, err := b.Upload(ctx, strings.NewReader("x"), 1, sha, "image/png")
		if err != nil || u != srv.URL+"/"+sha {
			t.Fatalf("upload = %q, %v", u, err)
		}
	}
	for i, ev := range got {
		if !ev.CheckID() || !ev.VerifySignature() || ev.Kind != 24242 || hashes[i] != strings.Repeat("ab", 32) {
			t.Errorf("auth %d: %+v (X-SHA-256 %q)", i, ev, hashes[i])
		}
		if ev.Tags.FindWithValue("x", sha) == nil || ev.Tags.FindWithValue("t", "upload") == nil || ev.Tags.Find("expiration") == nil {
			t.Errorf("auth %d tags = %v", i, ev.Tags)
		}
	}
	if got[0].PubKey != nostr.GetPublicKey(own) || got[0].Tags.FindWithValue("server", host) == nil {
		t.Errorf("own key: %v by %s", got[0].Tags, got[0].PubKey.Hex())
	}
	if got[1].PubKey != nostr.GetPublicKey(env) || got[1].Tags.Find("server") != nil {
		t.Errorf("env key: %v by %s", got[1].Tags, got[1].PubKey.Hex())
	}
}

// A server set in the app is trusted for its own address only: one it
// answers on another host becomes its own address for the hash.
func TestOwnBlobURL(t *testing.T) {
	base, _ := url.Parse("https://blossom.example/b")
	sha := strings.Repeat("cd", 32)
	cases := map[string]string{
		"https://blossom.example/" + sha + ".png":  "https://blossom.example/" + sha + ".png",
		"https://BLOSSOM.example/x/" + sha:         "https://BLOSSOM.example/x/" + sha,
		"https://cdn.example/" + sha + ".jpg":      "https://blossom.example/b/" + sha + ".jpg",
		"http://blossom.example/" + sha + ".png":   "https://blossom.example/b/" + sha + ".png",
		"https://u:p@blossom.example/" + sha:       "https://blossom.example/b/" + sha,
		"http://169.254.169.254/latest/meta-data/": "https://blossom.example/b/" + sha,
		"https://evil.example/" + sha + ".exe.php": "https://blossom.example/b/" + sha,
	}
	for in, want := range cases {
		if got := ownBlobURL(base, in, sha); got != want {
			t.Errorf("%s → %s, want %s", in, got, want)
		}
	}
	// The environment's server is trusted as before.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"https://cdn.example/` + sha + `"}`))
	}))
	defer srv.Close()
	for own, want := range map[bool]string{false: "https://cdn.example/" + sha, true: srv.URL + "/" + sha} {
		b := &Blossom{URL: srv.URL, Key: nostr.Generate(), Own: own, HTTP: srv.Client()}
		if got, err := b.Upload(context.Background(), strings.NewReader("x"), 1, sha, "image/png"); err != nil || got != want {
			t.Errorf("own %v: %q, %v; want %q", own, got, err, want)
		}
	}
}

// Without a store nothing is uploaded and the pipeline says so; a store
// that can't be used says why; the store is asked at each upload.
func TestPipelineFollowsTheStore(t *testing.T) {
	var cur Store
	var why error
	p := New(func() (Store, error) {
		if why != nil {
			return nil, why
		}
		if cur == nil {
			return nil, ErrNoStore
		}
		return cur, nil
	})
	ctx := context.Background()
	if p.Enabled() || p.Public() {
		t.Error("enabled without a store")
	}
	if _, err := p.Process(ctx, []byte("x"), "text/plain"); !errors.Is(err, ErrNoStore) {
		t.Errorf("err = %v", err)
	}
	why = &UnusableError{Reason: "no key"}
	var ue *UnusableError
	if _, err := p.Process(ctx, []byte("x"), "text/plain"); !errors.As(err, &ue) || !p.Enabled() {
		t.Errorf("err = %v, enabled %v", err, p.Enabled())
	}
	why = nil
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer srv.Close()
	cur = &Blossom{URL: srv.URL, Key: nostr.Generate(), HTTP: srv.Client()}
	if res, err := p.Process(ctx, []byte("x"), "text/plain"); err != nil || !strings.HasPrefix(res.URL, srv.URL+"/") {
		t.Errorf("res = %+v, %v", res, err)
	}
	if !p.Public() || p.BlossomBase() != srv.URL || !strings.Contains(p.Describe(), "the environment's Nostr key") {
		t.Errorf("public %v base %q describe %q", p.Public(), p.BlossomBase(), p.Describe())
	}
	var nilP *Pipeline
	if nilP.Enabled() || nilP.BlossomBase() != "" {
		t.Error("a nil pipeline is enabled")
	}
}

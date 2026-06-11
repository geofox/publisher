package media

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
)

func TestProcessUploadsAndBuildsImeta(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" || r.Method != http.MethodPut {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/abc"}`))
	}))
	defer srv.Close()

	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	res, err := p.Process(context.Background(), []byte("hello"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://blossom.example/abc" {
		t.Errorf("url = %q", res.URL)
	}
	if res.SHA256 == "" || res.Imeta[0] != "imeta" {
		t.Errorf("bad result: %+v", res)
	}
	joined := strings.Join(res.Imeta, " ")
	for _, want := range []string{"url https://blossom.example/abc", "m text/plain", "x " + res.SHA256} {
		if !strings.Contains(joined, want) {
			t.Errorf("imeta missing %q; got %v", want, res.Imeta)
		}
	}
}

func TestImetaTag(t *testing.T) {
	// Full tag: url/mime/x always present, dim/blurhash appended when set.
	full := ImetaTag("https://b/x", "image/png", "deadbeef", "640x480", "LEHV6")
	if strings.Join(full, " ") != "imeta url https://b/x m image/png x deadbeef dim 640x480 blurhash LEHV6" {
		t.Errorf("full tag wrong: %v", full)
	}
	// Optional fields omitted when empty (no trailing "dim "/"blurhash " entries).
	bare := ImetaTag("https://b/y", "image/jpeg", "cafe", "", "")
	if len(bare) != 4 {
		t.Errorf("bare tag should have 4 fields, got %d: %v", len(bare), bare)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("imgbytes"))
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New("http://unused", sk, nostr.GetPublicKey(sk))
	data, mime, err := p.Fetch(context.Background(), srv.URL+"/x.png")
	if err != nil || string(data) != "imgbytes" || mime != "image/png" {
		t.Errorf("fetch: data=%q mime=%q err=%v", data, mime, err)
	}
}

func TestProcessConvertsHEIC(t *testing.T) {
	b, err := os.ReadFile("../transcode/testdata/sample.heic")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" || r.Method != http.MethodPut {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/converted"}`))
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	res, err := p.Process(context.Background(), b, "image/heic")
	if err != nil {
		t.Fatal(err)
	}
	if res.Mime != "image/jpeg" {
		t.Fatalf("mime = %s, want image/jpeg (HEIC must never become canonical)", res.Mime)
	}
	sum := sha256.Sum256(res.Bytes)
	if hex.EncodeToString(sum[:]) != res.SHA256 {
		t.Fatal("SHA256 must hash the converted bytes, not the HEIC input")
	}
	if res.Dim == "" || res.Blurhash == "" {
		t.Fatalf("converted JPEG must yield dim+blurhash, got %+v", res)
	}
}

func TestProcessRejectsCorruptHEIC(t *testing.T) {
	corrupt := []byte("\x00\x00\x00\x18ftypheic garbage garbage")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/corrupt"}`))
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	if _, err := p.Process(context.Background(), corrupt, "image/heic"); err == nil {
		t.Fatal("corrupt HEIC must be rejected, not stored as canonical")
	}
}

package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	full := ImetaTag("https://b/x", "image/png", "deadbeef", "640x480", "LEHV6", "")
	if strings.Join(full, " ") != "imeta url https://b/x m image/png x deadbeef dim 640x480 blurhash LEHV6" {
		t.Errorf("full tag wrong: %v", full)
	}
	// Optional fields omitted when empty (no trailing "dim "/"blurhash " entries).
	bare := ImetaTag("https://b/y", "image/jpeg", "cafe", "", "", "")
	if len(bare) != 4 {
		t.Errorf("bare tag should have 4 fields, got %d: %v", len(bare), bare)
	}
}

func TestImetaTagImageField(t *testing.T) {
	tag := ImetaTag("https://b/v", "video/mp4", "aa", "1280x720", "", "https://b/poster")
	joined := strings.Join(tag, "|")
	if !strings.Contains(joined, "image https://b/poster") {
		t.Fatalf("imeta missing image field: %v", tag)
	}
	plain := ImetaTag("https://b/v", "video/mp4", "aa", "1280x720", "", "")
	if strings.Contains(strings.Join(plain, "|"), "image ") {
		t.Fatalf("empty poster must omit image: %v", plain)
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

func TestFetchCapEnforced(t *testing.T) {
	const smallCap = 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(bytes.Repeat([]byte("x"), smallCap+10))
	}))
	defer srv.Close()

	orig := fetchCapBytes
	fetchCapBytes = smallCap
	t.Cleanup(func() { fetchCapBytes = orig })

	sk := nostr.Generate()
	p := New("http://unused", sk, nostr.GetPublicKey(sk))
	_, _, err := p.Fetch(context.Background(), srv.URL+"/big")
	if err == nil {
		t.Fatal("expected error when response exceeds fetchCapBytes, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention 'exceeds', got: %v", err)
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

func TestProcessJPEGUntouched(t *testing.T) {
	// Re-fetch paths re-Process canonical objects; non-HEIC bytes must come
	// back byte-identical (sha of input == stored sha).
	img := image.NewRGBA(image.Rect(0, 0, 20, 20))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	src := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/upload" || r.Method != http.MethodPut {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://blossom.example/jpeg"}`))
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	res, err := p.Process(context.Background(), src, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(src)
	if res.SHA256 != hex.EncodeToString(sum[:]) || !bytes.Equal(res.Bytes, src) {
		t.Fatal("non-HEIC input must pass through Process byte-identical")
	}
}

func TestProcessRejectsCorruptHEIC(t *testing.T) {
	corrupt := []byte("\x00\x00\x00\x18ftypheic garbage garbage")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("corrupt HEIC must be rejected before any Blossom request, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	if _, err := p.Process(context.Background(), corrupt, "image/heic"); err == nil {
		t.Fatal("corrupt HEIC must be rejected, not stored as canonical")
	}
}

func TestProcessFileStreamsWithProgress(t *testing.T) {
	var received atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.Copy(io.Discard, r.Body)
		received.Store(n)
		json.NewEncoder(w).Encode(map[string]string{"url": "https://b/streamed"})
	}))
	defer srv.Close()

	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))

	f := filepath.Join(t.TempDir(), "v.mp4")
	payload := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB
	if err := os.WriteFile(f, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	var lastPct atomic.Int64 // stores frac*1e9 to avoid float races
	res, err := p.ProcessFile(context.Background(), f, "video/mp4", "1280x720", 42,
		func(frac float64) { lastPct.Store(int64(frac * 1e9)) })
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if res.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatal("sha must cover the file bytes")
	}
	if res.URL != "https://b/streamed" || res.Mime != "video/mp4" || res.Dim != "1280x720" {
		t.Fatalf("res = %+v", res)
	}
	if res.DurationSecs != 42 {
		t.Fatalf("duration = %d", res.DurationSecs)
	}
	if res.Bytes != nil {
		t.Fatal("ProcessFile must not retain bytes in RAM")
	}
	if received.Load() != int64(len(payload)) {
		t.Fatalf("server received %d bytes", received.Load())
	}
	if float64(lastPct.Load())/1e9 < 0.99 {
		t.Fatalf("progress last=%f", float64(lastPct.Load())/1e9)
	}
	joined := strings.Join(res.Imeta, "|")
	if !strings.Contains(joined, "m video/mp4") || !strings.Contains(joined, "dim 1280x720") {
		t.Fatalf("imeta = %v", res.Imeta)
	}
}

func TestProcessFileLookupShortCircuits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("lookup hit must not touch Blossom, got %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	sk := nostr.Generate()
	p := New(srv.URL, sk, nostr.GetPublicKey(sk))
	p.Lookup = func(sha string) (string, bool, error) { return "https://b/cached", true, nil }

	f := filepath.Join(t.TempDir(), "v.mp4")
	if err := os.WriteFile(f, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := p.ProcessFile(context.Background(), f, "video/mp4", "10x10", 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.URL != "https://b/cached" {
		t.Fatalf("url = %s", res.URL)
	}
}

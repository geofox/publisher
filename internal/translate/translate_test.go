package translate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIntersectPreservesOrderAndDropsUnsupported(t *testing.T) {
	got := Intersect([]string{"en", "fr", "nl", "lb"})
	want := []string{"en", "fr", "nl"} // lb (Luxembourgish) is not a DeepL target
	if len(got) != len(want) {
		t.Fatalf("Intersect = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Intersect[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsSupported(t *testing.T) {
	for _, c := range []string{"en", "fr", "FR", "nl", "pt"} {
		if !IsSupported(c) {
			t.Errorf("IsSupported(%q) = false, want true", c)
		}
	}
	for _, c := range []string{"lb", "", "xx"} {
		if IsSupported(c) {
			t.Errorf("IsSupported(%q) = true, want false", c)
		}
	}
}

func TestNewDeepLDetectsFreeKey(t *testing.T) {
	if c := NewDeepL("abc:fx"); c.baseURL != "https://api-free.deepl.com" {
		t.Errorf("free key → baseURL = %q, want api-free", c.baseURL)
	}
	if c := NewDeepL("abc"); c.baseURL != "https://api.deepl.com" {
		t.Errorf("pro key → baseURL = %q, want api.deepl.com", c.baseURL)
	}
}

func TestTranslateSendsCorrectRequestAndDecodes(t *testing.T) {
	var gotAuth, gotCT, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/translate" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"translations": []map[string]string{{
				"detected_source_language": "EN",
				"text":                     "Bonjour le monde",
			}},
		})
	}))
	defer srv.Close()

	c := NewDeepLWithBase("k123", srv.URL)
	got, src, err := c.Translate(context.Background(), "Hello world", "fr")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bonjour le monde" {
		t.Errorf("translated = %q", got)
	}
	if src != "en" {
		t.Errorf("detected = %q, want en (lowercased)", src)
	}
	if gotAuth != "DeepL-Auth-Key k123" {
		t.Errorf("auth header = %q", gotAuth)
	}
	if !strings.HasPrefix(gotCT, "application/x-www-form-urlencoded") {
		t.Errorf("content-type = %q", gotCT)
	}
	// target_lang must be DeepL's wire value (FR), not the ISO 639-1 lowercase we accept.
	if !strings.Contains(gotBody, "target_lang=FR") || !strings.Contains(gotBody, "text=Hello+world") {
		t.Errorf("body = %q, want target_lang=FR and text=Hello+world", gotBody)
	}
}

func TestTranslateSendsPTPTForPortuguese(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"translations": []map[string]string{{"detected_source_language": "EN", "text": "ola"}},
		})
	}))
	defer srv.Close()
	c := NewDeepLWithBase("k", srv.URL)
	if _, _, err := c.Translate(context.Background(), "hi", "pt"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotBody, "target_lang=PT-PT") {
		t.Errorf("pt should map to PT-PT, got %q", gotBody)
	}
}

func TestTranslateRejectsUnsupportedTarget(t *testing.T) {
	c := NewDeepLWithBase("k", "http://example.invalid")
	if _, _, err := c.Translate(context.Background(), "hi", "lb"); err == nil {
		t.Error("translating to lb should error (not a DeepL target)")
	}
}

func TestTranslateRejectsEmptyText(t *testing.T) {
	c := NewDeepLWithBase("k", "http://example.invalid")
	if _, _, err := c.Translate(context.Background(), "   ", "fr"); err == nil {
		t.Error("empty text should error")
	}
}

func TestTranslateSurfacesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"quota exceeded"}`))
	}))
	defer srv.Close()
	c := NewDeepLWithBase("k", srv.URL)
	_, _, err := c.Translate(context.Background(), "hi", "fr")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 error, got %v", err)
	}
}

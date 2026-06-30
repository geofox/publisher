package api

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fiatjaf.com/nostr"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
)

type fakeDispatcher struct{}

func (fakeDispatcher) Post(ctx context.Context, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: "p1", Status: "success", Platforms: spec.Platforms}
}

func (fakeDispatcher) Retry(ctx context.Context, id string, platforms []string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (fakeDispatcher) RetryRelay(ctx context.Context, id, relay string) (*store.Post, error) {
	return &store.Post{ID: id, Status: "success"}, nil
}
func (fakeDispatcher) Schedule(ctx context.Context, spec dispatch.PostSpec, at time.Time) (*store.Post, error) {
	return &store.Post{ID: "sch", Status: "scheduled"}, nil
}
func (fakeDispatcher) PostWithID(_ context.Context, id string, spec dispatch.PostSpec) *store.Post {
	return &store.Post{ID: id, Status: "success", Platforms: spec.Platforms}
}
func (fakeDispatcher) Interact(context.Context, dispatch.InteractSpec) *store.Post { return nil }
func (fakeDispatcher) InteractWithID(_ context.Context, id string, spec dispatch.InteractSpec) *store.Post {
	return &store.Post{ID: id, Status: "success"}
}

func TestAPIPost(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{
		"master_text": "hi", "platforms": []string{"nostr", "mastodon"},
	})
	_ = mw.WriteField("spec", string(spec))
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["post_id"] != "p1" {
		t.Errorf("resp = %v", resp)
	}
}

func TestAPIPostTooManyImages(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{"master_text": "hi", "platforms": []string{"nostr"}})
	_ = mw.WriteField("spec", string(spec))
	for i := 0; i < 41; i++ {
		fw, _ := mw.CreateFormFile("image", "x.png")
		_, _ = fw.Write([]byte("img"))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("41 images: code = %d, want 400", rec.Code)
	}
}

func TestAPIPostMissingSpec(t *testing.T) {
	a := &API{Dispatch: fakeDispatcher{}}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("notspec", "{}")
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing spec: code = %d, want 400", rec.Code)
	}
}

func TestAPIPostMediaError(t *testing.T) {
	// Blossom stub that always 500s → media.Process fails → handler returns 502.
	blossom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer blossom.Close()
	sk := nostr.Generate()
	a := &API{Dispatch: fakeDispatcher{}}
	a.media = media.New(blossom.URL, sk, nostr.GetPublicKey(sk))

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	spec, _ := json.Marshal(map[string]any{"master_text": "hi", "platforms": []string{"nostr"}})
	_ = mw.WriteField("spec", string(spec))
	fw, _ := mw.CreateFormFile("image", "x.png")
	_, _ = fw.Write([]byte("img"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/post", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("media error: code = %d, want 502", rec.Code)
	}
}

func TestHandleConfigReturnsUserLanguages(t *testing.T) {
	a := &API{UserLanguages: []string{"en", "fr"}}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got struct {
		UserLanguages    []string `json:"user_languages"`
		TranslateTargets []string `json:"translate_targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.UserLanguages) != 2 || got.UserLanguages[0] != "en" || got.UserLanguages[1] != "fr" {
		t.Errorf("user_languages = %v, want [en fr]", got.UserLanguages)
	}
	// No Translator configured → translate_targets must be empty (UI hides button).
	if len(got.TranslateTargets) != 0 {
		t.Errorf("translate_targets without Translator = %v, want []", got.TranslateTargets)
	}
}

func TestHandleConfigUnsetEmitsEmptyArrays(t *testing.T) {
	a := &API{} // UserLanguages nil, Translator nil
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	// Both lists must marshal as arrays (not null) so the JS treats them uniformly.
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte(`"user_languages":[]`)) {
		t.Errorf("body = %s, want user_languages:[]", body)
	}
	if !bytes.Contains([]byte(body), []byte(`"translate_targets":[]`)) {
		t.Errorf("body = %s, want translate_targets:[]", body)
	}
}

// fakeTranslator records its inputs and returns canned output. Lets the API
// handler tests run without touching DeepL.
type fakeTranslator struct {
	gotText, gotTarget string
	out, src           string
	err                error
}

func (f *fakeTranslator) Translate(_ context.Context, text, target string) (string, string, error) {
	f.gotText, f.gotTarget = text, target
	if f.err != nil {
		return "", "", f.err
	}
	return f.out, f.src, nil
}

func TestHandleConfigIntersectsTranslateTargets(t *testing.T) {
	// User has en,fr,nl,lb; DeepL doesn't support lb, so translate_targets = en,fr,nl.
	a := &API{UserLanguages: []string{"en", "fr", "nl", "lb"}, Translator: &fakeTranslator{}}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		TranslateTargets []string `json:"translate_targets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"en", "fr", "nl"}
	if len(got.TranslateTargets) != len(want) {
		t.Fatalf("translate_targets = %v, want %v", got.TranslateTargets, want)
	}
	for i := range want {
		if got.TranslateTargets[i] != want[i] {
			t.Errorf("[%d] = %q, want %q", i, got.TranslateTargets[i], want[i])
		}
	}
}

func TestHandleTranslateHappy(t *testing.T) {
	tr := &fakeTranslator{out: "Bonjour", src: "en"}
	a := &API{Translator: tr}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/translate",
		bytes.NewReader([]byte(`{"text":"Hello","target_lang":"fr"}`)))
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if tr.gotText != "Hello" || tr.gotTarget != "fr" {
		t.Errorf("translator got text=%q target=%q", tr.gotText, tr.gotTarget)
	}
	var got struct {
		Text                   string `json:"text"`
		DetectedSourceLanguage string `json:"detected_source_language"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "Bonjour" || got.DetectedSourceLanguage != "en" {
		t.Errorf("response = %+v", got)
	}
}

func TestHandleTranslateDisabledWhenNoTranslator(t *testing.T) {
	a := &API{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/translate",
		bytes.NewReader([]byte(`{"text":"hi","target_lang":"fr"}`)))
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestHandleTranslateRejectsUnsupportedTarget(t *testing.T) {
	a := &API{Translator: &fakeTranslator{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/translate",
		bytes.NewReader([]byte(`{"text":"hi","target_lang":"lb"}`)))
	a.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

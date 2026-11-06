package unfurl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
	"unicode/utf8"
)

// The composer preview gives Unfurl 5 s; dispatch gives it 15 s. A slow page
// that times out the preview is negatively cached for a minute, so a publish
// right after the preview posts without its link card.
func TestCallerTimeoutNotNegativelyCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><meta property="og:title" content="Slow page"/></head></html>`))
	}))
	defer srv.Close()
	s := &Service{HTTP: srv.Client(), cache: map[string]cacheEntry{}}

	pctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, perr := s.Unfurl(pctx, srv.URL+"/a")
	cancel()
	if perr == nil {
		t.Fatal("preview should have timed out")
	}
	dctx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	card, err := s.Unfurl(dctx, srv.URL+"/a")
	if err != nil || card == nil {
		t.Errorf("publish-time unfurl = %v, %v; the preview's own timeout was cached", card, err)
	}
}

// A page served as ISO-8859-1 yields a title that is not valid UTF-8; JSON
// encoding then replaces the bytes with U+FFFD in the Bluesky card.
func TestTitleDecodedFromDeclaredCharset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=iso-8859-1")
		_, _ = w.Write([]byte("<html><head><title>Caf\xe9 cr\xe8me</title></head></html>"))
	}))
	defer srv.Close()
	s := &Service{HTTP: srv.Client(), cache: map[string]cacheEntry{}}
	card, err := s.Unfurl(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(card.Title) || card.Title != "Café crème" {
		t.Errorf("title = %q (valid UTF-8: %v), want %q", card.Title, utf8.ValidString(card.Title), "Café crème")
	}
}

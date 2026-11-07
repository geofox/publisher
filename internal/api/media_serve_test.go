package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/settings"
	"github.com/geofox/publisher/internal/store"
)

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// roomyDisk is a volume with room to spare.
func roomyDisk(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil }

// diskMedia gives the env a disk store at base and puts body in it.
func (e *v1Env) diskMedia(base string, public *bool) *media.Local {
	e.t.Helper()
	l := &media.Local{Dir: filepath.Join(e.t.TempDir(), "media"), BaseURL: base, Free: roomyDisk,
		IsPublic: func() bool { return public != nil && *public }}
	e.api.DiskMedia = l
	return l
}

func (e *v1Env) raw(method, path string, header map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	r := httptest.NewRequest(method, path, nil)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// Media on this server's disk are served without sign-in at
// /media/<sha256>.<ext>: images and videos as their type, anything else
// as an attachment, under a policy that runs nothing; any other name is
// not found, and nothing under /media/ is the web app.
func TestServeDiskMedia(t *testing.T) {
	ctx := context.Background()
	e := newV1Env(t, true) // sign-in on: the web app is gated
	if w := e.raw("GET", "/media/"+strings.Repeat("a", 64)+".png", nil); w.Code != 404 {
		t.Fatalf("no disk store: %d", w.Code)
	}
	l := e.diskMedia("", nil)
	img := pngBytes(t, 4, 4)
	sha := hexSHA(img)
	if _, err := l.Upload(ctx, bytes.NewReader(img), int64(len(img)), sha, "image/png"); err != nil {
		t.Fatal(err)
	}
	page := []byte("<!doctype html><script>alert(document.cookie)</script>")
	pageSHA := hexSHA(page)
	if u, err := l.Upload(ctx, bytes.NewReader(page), int64(len(page)), pageSHA, "image/png"); err != nil || !strings.HasSuffix(u, ".bin") {
		t.Fatalf("page = %q, %v", u, err)
	}

	w := e.raw("GET", "/media/"+sha+".png", nil)
	h := w.Header()
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), img) {
		t.Fatalf("GET = %d (%d bytes)", w.Code, w.Body.Len())
	}
	for k, want := range map[string]string{
		"Content-Type":                 "image/png",
		"Content-Security-Policy":      "default-src 'none'; sandbox",
		"X-Content-Type-Options":       "nosniff",
		"Cache-Control":                "public, max-age=31536000, immutable",
		"ETag":                         `"` + sha + `"`,
		"Cross-Origin-Resource-Policy": "cross-origin",
		"Access-Control-Allow-Origin":  "*",
		"Content-Length":               strconv.Itoa(len(img)),
		"Content-Disposition":          "",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if w := e.raw("HEAD", "/media/"+sha+".png", nil); w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != strconv.Itoa(len(img)) {
		t.Errorf("HEAD = %d, %d bytes, length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	if w := e.raw("GET", "/media/"+sha+".png", map[string]string{"Range": "bytes=0-3"}); w.Code != 206 || !bytes.Equal(w.Body.Bytes(), img[:4]) {
		t.Errorf("range = %d %q", w.Code, w.Body.Bytes())
	}
	if w := e.raw("GET", "/media/"+sha+".png", map[string]string{"If-None-Match": `"` + sha + `"`}); w.Code != 304 {
		t.Errorf("revalidate = %d", w.Code)
	}
	w = e.raw("GET", "/media/"+pageSHA+".bin", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/octet-stream" ||
		w.Header().Get("Content-Disposition") != `attachment; filename="`+pageSHA+`.bin"` ||
		w.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" {
		t.Errorf("page = %d %v", w.Code, w.Header())
	}
	if w := e.raw("POST", "/media/"+sha+".png", nil); w.Code != 405 && w.Code != 403 {
		t.Errorf("POST = %d", w.Code)
	}

	// A file the store didn't write is never served, whatever its name.
	if err := os.WriteFile(filepath.Join(l.Dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"/media/" + sha + ".jpg",      // not the type it was kept as
		"/media/" + pageSHA + ".png",  // the page, as an image
		"/media/" + pageSHA + ".html", // the page, as a page
		"/media/" + sha,               // no extension
		"/media/" + strings.ToUpper(sha) + ".png",
		"/media/" + sha + ".png/x",
		"/media/x/y",
		"/media/notes.txt",
		"/media/",
	} {
		w := e.raw("GET", p, nil)
		if w.Code != 404 || strings.Contains(w.Body.String(), "<html") || strings.Contains(w.Body.String(), "<!doctype") {
			t.Errorf("%s = %d %q", p, w.Code, w.Body.String())
		}
	}
	// A path that leaves /media/ is redirected to where it leads, never
	// served from the media directory.
	for _, p := range []string{"/media/../t.db", "/media/%2e%2e%2ft.db", "/media/..%2ft.db", "/media/../index.html"} {
		if w := e.raw("GET", p, nil); w.Code == 200 {
			t.Errorf("%s = %d", p, w.Code)
		}
	}
	// The gate stays shut for the web app beside it.
	if w := e.raw("GET", "/", nil); w.Code == 200 {
		t.Errorf("GET / without a session = %d", w.Code)
	}
}

// An upload goes to this server's disk when it is the store, its address
// is on PUBLIC_URL; one that wouldn't fit is refused as no room.
func TestV1UploadToDisk(t *testing.T) {
	e := newV1Env(t, true)
	l := e.diskMedia("https://pub.example", nil)
	e.api.media = media.New(media.Static(l))
	img := pngBytes(t, 8, 4)
	r := e.do(req{method: "POST", path: "/v1/media", body: string(img), header: map[string]string{"Content-Type": "image/png"}})
	wantStatus(t, r, 201)
	var m v1MediaObjectBody
	r.json(&m)
	if m.URL == nil || *m.URL != "https://pub.example/media/"+hexSHA(img)+".png" {
		t.Fatalf("url = %v", m.URL)
	}
	l.Quota = func() int64 { return 1 }
	p := wantProblem(t, e.do(req{method: "POST", path: "/v1/media", body: string(pngBytes(t, 9, 4)),
		header: map[string]string{"Content-Type": "image/png"}}), 507, codeNoSpace)
	if !strings.Contains(p.Detail, "Settings, Media") {
		t.Errorf("detail = %q", p.Detail)
	}
}

// A network that fetches media by address (Nostr, Threads) is refused
// this server's disk media unless they are served publicly; one that
// takes uploads is not, nor a Blossom server's media.
func TestV1DiskMediaNeedsPublicForURLNetworks(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.seedAccounts("nostr")
	public := false
	l := e.diskMedia("https://pub.example", &public)
	object := func(u string) string {
		m := &store.MediaObject{OwnerID: store.DefaultOwner, Status: store.MediaReady, Mime: "image/png", Dim: "8x4",
			SizeBytes: 100, URL: u, SHA256: strings.Repeat("e", 64)}
		if err := e.st.CreateMediaObject(context.Background(), m); err != nil {
			t.Fatal(err)
		}
		return m.ID
	}
	local := object(l.URL(strings.Repeat("e", 64) + ".png"))
	blossom := object("https://blossom.test/" + strings.Repeat("e", 64))
	post := func(med, acct string) req {
		return req{method: "POST", path: "/v1/posts:preview", body: `{"content":{"text":"x","media":[{"id":"` + med + `","alt":"a"}]},
			"targets":[{"account":"` + e.acct(acct) + `"}]}`}
	}
	p := wantProblem(t, e.do(post(local, "nostr")), 422, codeValidation)
	if c := fieldCodes(p); c != "/content/media/0:media_unsupported" || !strings.Contains(p.Errors[0].Detail, "Settings, Media") {
		t.Fatalf("nostr, not public: %s %+v", c, p.Errors)
	}
	wantStatus(t, e.do(post(local, "bluesky")), 200)
	wantStatus(t, e.do(post(blossom, "nostr")), 200)
	public = true
	wantStatus(t, e.do(post(local, "nostr")), 200)
}

// Turning media.public on runs the self-test, through PUBLIC_URL as a
// network would: it passes, or the setting stays off and the answer says
// what PUBLIC_URL answered. The test counts against the tests' limit.
func TestV1MediaPublicSelfTest(t *testing.T) {
	e := newSettingsEnv(t, true, nil)
	app := httptest.NewServer(e.h)
	defer app.Close()
	public := func() bool { return e.svc.Current().Bool(settings.MediaPublic) }
	l := &media.Local{Dir: t.TempDir(), Free: roomyDisk, IsPublic: public}
	e.api.DiskMedia = l
	on := req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"public":true}}`}
	off := req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"public":false}}`}
	refused := func(why string, want string) {
		t.Helper()
		e.answer(204, nil) // past the limiter's window
		p := wantProblem(t, e.do(on), 422, codeValidation)
		if c := fieldCodes(p); c != "/media/public:not_reachable" || !strings.Contains(p.Errors[0].Detail, want) {
			t.Errorf("%s: %s %+v", why, c, p.Errors)
		}
		if public() {
			t.Errorf("%s: turned on", why)
		}
	}
	refused("no PUBLIC_URL", "set PUBLIC_URL first")

	mode := 0
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case 302:
			http.Redirect(w, r, "https://sso.example/login", http.StatusFound)
		case 401:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			_, _ = w.Write([]byte("<html>the web app</html>"))
		}
	}))
	defer front.Close()
	l.BaseURL = front.URL
	mode = 302
	refused("a sign-in in front", "redirect (302)")
	mode = 401
	refused("forward auth", "answered 401")
	mode = 200
	refused("another app", "something other than the file")
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	l.BaseURL = gone.URL
	refused("unreachable", "couldn't be reached from this server (refused)")
	if entries, _ := os.ReadDir(l.Dir); len(entries) != 0 {
		t.Errorf("probes left: %d", len(entries))
	}

	// Through the app itself: it passes and the setting is on.
	l.BaseURL = app.URL
	e.answer(204, nil)
	wantStatus(t, e.do(on), 200)
	if !public() || !l.Public() {
		t.Fatal("not on")
	}
	// Already on: no test. Off needs none; on again within the window is
	// limited, as a test is.
	wantStatus(t, e.do(on), 200)
	wantStatus(t, e.do(off), 200)
	wantProblem(t, e.do(on), 429, codeRateLimited)
	e.answer(204, nil)
	wantStatus(t, e.do(on), 200)
	// A write that leaves it alone doesn't run it.
	l.BaseURL = gone.URL
	wantStatus(t, e.do(req{method: "PATCH", path: "/v1/instance/settings", body: `{"media":{"local_quota":10}}`}), 200)
	if entries, _ := os.ReadDir(l.Dir); len(entries) != 0 {
		t.Errorf("probes left: %d", len(entries))
	}
}

// A quote's link-quotes carry the original's images copied to this
// server's disk, except on a network that fetches media by address while
// they aren't served publicly: that one goes without, and says so.
func TestV1QuoteCopiesOnDisk(t *testing.T) {
	e := newV1Env(t, true)
	e.withEngine()
	e.seedAccounts("nostr")
	e.withAccounts()
	l := e.diskMedia("https://pub.example", nil)
	e.api.media = media.New(media.Static(l))
	img := pngBytes(t, 4, 4)
	e.api.fetchMedia = func(context.Context, string) ([]byte, string, error) { return img, "image/png", nil }
	bsky, masto, nostr := e.acct("bluesky"), e.acct("mastodon"), e.acct("nostr")
	r := e.do(req{method: "POST", path: "/v1/posts", body: `{"content":{"text":"look"},"targets":[{"account":"` + bsky +
		`"},{"account":"` + masto + `"},{"account":"` + nostr + `"}],"quote":{"url":"` + bskySource + `"}}`})
	wantStatus(t, r, 202)
	var body v1PostBody
	r.json(&body)
	p, err := e.st.GetPost(strings.TrimPrefix(body.ID, prefixPost))
	if err != nil {
		t.Fatal(err)
	}
	attached := map[string]int{}
	for _, tg := range p.Targets {
		for _, it := range tg.Items {
			attached[tg.AccountID] += len(it.Attach)
		}
	}
	if attached[masto] != 1 || attached[nostr] != 0 || attached[bsky] != 0 {
		t.Fatalf("attached = %v", attached)
	}
	for _, tg := range body.Targets {
		warned := strings.Contains(strings.Join(tg.Warnings, " "), "the original's images aren't attached")
		if warned != (tg.Account == nostr) {
			t.Errorf("%s warnings = %q", tg.Account, tg.Warnings)
		}
	}
}

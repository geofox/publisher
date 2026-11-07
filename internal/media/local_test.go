package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func shaOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// roomy is a volume with room to spare.
func roomy(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil }

// stored lists the media dir, temp files included.
func stored(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// An upload is kept as <sha256>.<ext> (0600 in a 0700 directory), the
// extension the type its bytes are; given a type they aren't, or one
// outside the served types, it is kept as .bin. Its address is on
// BaseURL, and Fetch reads it back from disk.
func TestLocalUpload(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "media")
	l := &Local{Dir: dir, BaseURL: "https://pub.example/", Free: roomy}
	img := testPNG(t)
	sha := shaOf(img)
	u, err := l.Upload(ctx, bytes.NewReader(img), int64(len(img)), sha, "image/png")
	if err != nil || u != "https://pub.example/media/"+sha+".png" {
		t.Fatalf("upload = %q, %v", u, err)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, %v", fi.Mode(), err)
	}
	if fi, err := os.Stat(filepath.Join(dir, sha+".png")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, %v", fi.Mode(), err)
	}
	if b, mime, err := l.Fetch(ctx, u); err != nil || !bytes.Equal(b, img) || mime != "image/png" {
		t.Errorf("fetch = %d bytes %q, %v", len(b), mime, err)
	}
	// Stored already: the same address, even without room for another.
	full := &Local{Dir: dir, BaseURL: l.BaseURL, Free: func(string) (uint64, uint64, error) { return 0, 100 << 30, nil }}
	if again, err := full.Upload(ctx, bytes.NewReader(img), int64(len(img)), sha, "image/png"); err != nil || again != u {
		t.Errorf("again = %q, %v", again, err)
	}

	ftyp := func(brand string) []byte {
		return append([]byte{0, 0, 0, 0x18, 'f', 't', 'y', 'p'}, []byte(brand+"\x00\x00\x02\x00isomiso2")...)
	}
	cases := []struct {
		name  string
		body  []byte
		given string
		ext   string
	}{
		{"png, said so", img, "image/png", "png"},
		{"png, untyped", img, "", "png"},
		{"png, said jpeg", img, "image/jpeg", "bin"},
		{"html, said png", []byte("<!doctype html><script>alert(1)</script>"), "image/png", "bin"},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "image/svg+xml", "bin"},
		{"mp4", ftyp("isom"), "video/mp4", "mp4"},
		{"quicktime", ftyp("qt  "), "video/quicktime", "bin"},
		{"webm", []byte{0x1a, 0x45, 0xdf, 0xa3, 0x9f, 0x42, 0x86, 0x81, 0x01}, "video/webm", "webm"},
		{"text", []byte("hello"), "text/plain", "bin"},
	}
	for _, c := range cases {
		d := t.TempDir()
		cl := &Local{Dir: d, Free: roomy}
		s := shaOf(c.body)
		u, err := cl.Upload(ctx, bytes.NewReader(c.body), int64(len(c.body)), s, c.given)
		if err != nil || u != "/media/"+s+"."+c.ext {
			t.Errorf("%s: %q, %v; want .%s", c.name, u, err, c.ext)
		}
	}

	// Bytes that aren't the hash given leave nothing behind.
	d := t.TempDir()
	cl := &Local{Dir: d, Free: roomy}
	if _, err := cl.Upload(ctx, strings.NewReader("other"), 5, sha, "image/png"); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Errorf("wrong hash: %v", err)
	}
	if _, err := cl.Upload(ctx, strings.NewReader("x"), 1, "../../etc/passwd", "image/png"); err == nil {
		t.Error("a name that isn't a hash was taken")
	}
	if got := stored(t, d); len(got) != 0 {
		t.Errorf("left behind: %v", got)
	}
}

// Only names this store makes are served or read: no path is made from
// anything else.
func TestLocalNames(t *testing.T) {
	sha := strings.Repeat("ab", 32)
	cases := map[string]string{
		sha + ".png":  "image/png",
		sha + ".jpg":  "image/jpeg",
		sha + ".webm": "video/webm",
		sha + ".bin":  "application/octet-stream",
	}
	for name, want := range cases {
		if mime, attach, ok := TypeOf(name); !ok || mime != want || attach != (want == "application/octet-stream") {
			t.Errorf("%s: %q %v %v", name, mime, attach, ok)
		}
	}
	l := &Local{Dir: t.TempDir(), BaseURL: "https://pub.example"}
	// A file outside Dir that a name climbing out of it would reach.
	if err := os.WriteFile(filepath.Join(filepath.Dir(l.Dir), sha+".png"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", sha, sha + ".", sha + ".PNG", strings.ToUpper(sha) + ".png", sha + ".html", sha + ".svg",
		sha + ".png/x", "../" + sha + ".png", sha[:63] + ".png", sha + "0.png", sha + ".png.bin", ".upload-1"} {
		if _, _, ok := TypeOf(bad); ok {
			t.Errorf("%q taken", bad)
		}
		if f, _, _, err := l.Open(bad); !errors.Is(err, fs.ErrNotExist) {
			if f != nil {
				f.Close()
			}
			t.Errorf("%q opened: %v", bad, err)
		}
		if l.Has(bad) {
			t.Errorf("%q found", bad)
		}
	}
	for raw, want := range map[string]bool{
		"https://pub.example/media/" + sha + ".png":       true,
		"https://PUB.example/media/" + sha + ".png":       true,
		"/media/" + sha + ".png":                          true,
		"http://pub.example/media/" + sha + ".png":        false,
		"https://other.example/media/" + sha + ".png":     false,
		"https://u@pub.example/media/" + sha + ".png":     false,
		"https://pub.example/media/" + sha + ".png?x=1":   false,
		"https://pub.example/media/" + sha + ".png#x":     false,
		"https://pub.example/x/media/" + sha + ".png":     false,
		"https://pub.example/media/../" + sha + ".png":    false,
		"https://blossom.example/" + sha + ".png":         false,
		"https://pub.example/media/" + sha + ".html":      false,
		"https://pub.example/media/" + sha + ".png/../db": false,
	} {
		if _, got := l.Owns(raw); got != want {
			t.Errorf("Owns(%s) = %v, want %v", raw, got, want)
		}
	}
	// Without PUBLIC_URL only relative addresses are its own.
	rel := &Local{Dir: t.TempDir()}
	if _, ok := rel.Owns("https://pub.example/media/" + sha + ".png"); ok {
		t.Error("an absolute address is its own without a base")
	}
	if rel.URL(sha+".png") != "/media/"+sha+".png" {
		t.Errorf("URL = %q", rel.URL(sha+".png"))
	}
	// Public needs the setting and a public address.
	on := func() bool { return true }
	if rel.Public() || (&Local{BaseURL: "https://pub.example", IsPublic: on}).Public() != true ||
		(&Local{IsPublic: on}).Public() || (&Local{BaseURL: "https://pub.example"}).Public() {
		t.Error("Public")
	}
}

// Uploads leave a free-space floor (1 GiB, or 5 % of a large volume) and
// stay under the quota, counting the stored media only.
func TestLocalSpace(t *testing.T) {
	ctx := context.Background()
	var free, total uint64
	l := &Local{Dir: t.TempDir(), Free: func(string) (uint64, uint64, error) { return free, total, nil }}
	put := func(body string) error {
		_, err := l.Upload(ctx, strings.NewReader(body), int64(len(body)), shaOf([]byte(body)), "text/plain")
		return err
	}
	free, total = 1<<30+100, 10<<30
	if err := put("fits under the floor"); err != nil {
		t.Fatalf("under the floor: %v", err)
	}
	free = 1<<30 + 5
	err := put("past the floor")
	var ns *NoSpaceError
	if !errors.Is(err, ErrNoSpace) || !errors.As(err, &ns) || !strings.Contains(err.Error(), "1024 MiB of the disk free") {
		t.Fatalf("floor: %v", err)
	}
	// 5 % of 100 GiB is more than 1 GiB.
	free, total = 4<<30, 100<<30
	if err := put("five percent"); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("5%%: %v", err)
	}
	free = 6 << 30
	if err := put("room again"); err != nil {
		t.Fatal(err)
	}
	// Quota: what is stored counts, other files don't.
	if err := os.WriteFile(filepath.Join(l.Dir, "notes.txt"), make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}
	used := int64(len("fits under the floor") + len("room again"))
	l.Quota = func() int64 { return used + 10 }
	if err := put("ten bytes!"); err != nil {
		t.Fatalf("at the quota: %v", err)
	}
	if err := put("x"); !errors.As(err, &ns) || !strings.Contains(err.Error(), "Settings, Media") {
		t.Fatalf("past the quota: %v", err)
	}
	l.Quota = func() int64 { return 0 }
	if err := put("x"); err != nil {
		t.Fatalf("no quota: %v", err)
	}
	l.Free = func(string) (uint64, uint64, error) { return 0, 0, errors.New("statfs broke") }
	if err := put("y"); err == nil || errors.Is(err, ErrNoSpace) {
		t.Fatalf("statfs error: %v", err)
	}
	// The real statfs answers for a real directory.
	if f, tot, err := statfs(l.Dir); err != nil || tot == 0 || f > tot {
		t.Errorf("statfs = %d/%d, %v", f, tot, err)
	}
}

type recordFetcher struct{ got []string }

func (r *recordFetcher) Fetch(_ context.Context, u string) ([]byte, string, error) {
	r.got = append(r.got, u)
	return []byte("remote"), "image/png", nil
}

// This server's media are read from disk; anything else goes on.
func TestLocalFirst(t *testing.T) {
	ctx := context.Background()
	l := &Local{Dir: t.TempDir(), BaseURL: "https://pub.example", Free: roomy}
	img := testPNG(t)
	u, err := l.Upload(ctx, bytes.NewReader(img), int64(len(img)), shaOf(img), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	next := &recordFetcher{}
	f := LocalFirst{Local: l, Next: next}
	if b, _, err := f.Fetch(ctx, u); err != nil || !bytes.Equal(b, img) {
		t.Errorf("local: %d bytes, %v", len(b), err)
	}
	if _, _, err := f.Fetch(ctx, "https://pub.example/media/"+strings.Repeat("0", 64)+".png"); err == nil {
		t.Error("a missing local object was fetched")
	}
	if b, _, err := f.Fetch(ctx, "https://blossom.example/x"); err != nil || string(b) != "remote" {
		t.Errorf("remote: %q, %v", b, err)
	}
	if len(next.got) != 1 || next.got[0] != "https://blossom.example/x" {
		t.Errorf("next fetched %v", next.got)
	}
	if b, _, err := (LocalFirst{Next: next}).Fetch(ctx, u); err != nil || string(b) != "remote" {
		t.Errorf("no local store: %q, %v", b, err)
	}
}

// The public self-test fetches a probe at BaseURL/media/ and needs it
// back whole with a 200: a redirect (a sign-in page), a 401, other bytes
// or no PUBLIC_URL fail it, and the probe is removed either way.
func TestCheckPublic(t *testing.T) {
	ctx := context.Background()
	l := &Local{Dir: t.TempDir(), Free: roomy}
	mode := "ok"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "redirect":
			http.Redirect(w, r, "/login", http.StatusFound)
		case "401":
			w.WriteHeader(http.StatusUnauthorized)
		case "other":
			_, _ = w.Write([]byte("<html>sign in</html>"))
		case "404":
			http.NotFound(w, r)
		default:
			f, _, _, err := l.Open(strings.TrimPrefix(r.URL.Path, "/media/"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			http.ServeContent(w, r, "", time.Time{}, f)
		}
	}))
	defer srv.Close()
	if err := l.CheckPublic(ctx, NoRedirects()); !errors.Is(err, ErrNoPublicURL) {
		t.Fatalf("no base: %v", err)
	}
	l.BaseURL = srv.URL
	if err := l.CheckPublic(ctx, NoRedirects()); err != nil {
		t.Fatalf("ok: %v", err)
	}
	for m, want := range map[string]string{"redirect": "redirect (302)", "401": "answered 401: a sign-in", "other": "something other", "404": "answered 404"} {
		mode = m
		err := l.CheckPublic(ctx, NoRedirects())
		var pe *ProbeError
		if !errors.As(err, &pe) || !strings.Contains(err.Error(), want) || !strings.HasPrefix(err.Error(), srv.URL+"/media/") {
			t.Errorf("%s: %v", m, err)
		}
	}
	if got := stored(t, l.Dir); len(got) != 0 {
		t.Errorf("probes left: %v", got)
	}
	// No room for the probe: nothing is fetched.
	full := &Local{Dir: t.TempDir(), BaseURL: srv.URL, Free: func(string) (uint64, uint64, error) { return 0, 1 << 40, nil }}
	var pe *ProbeError
	if err := full.CheckPublic(ctx, NoRedirects()); !errors.As(err, &pe) || pe.Store == nil || !errors.Is(err, ErrNoSpace) {
		t.Errorf("no room: %v", err)
	}
	// Unreachable: the transport's error.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	l.BaseURL = gone.URL
	if err := l.CheckPublic(ctx, NoRedirects()); err == nil || errors.As(err, &pe) {
		t.Errorf("unreachable: %v", err)
	}
}

// The local check writes a probe and reads it back, leaving nothing.
func TestCheckWritable(t *testing.T) {
	ctx := context.Background()
	l := &Local{Dir: t.TempDir(), Free: roomy}
	if err := l.CheckWritable(ctx); err != nil {
		t.Fatal(err)
	}
	if got := stored(t, l.Dir); len(got) != 0 {
		t.Errorf("left: %v", got)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (&Local{Dir: filepath.Join(file, "media"), Free: roomy}).CheckWritable(ctx); err == nil {
		t.Error("a directory under a file was writable")
	}
}

// A link put in the directory under a stored object's name is never
// followed; an upload's temporary files left by a stop are removed.
func TestLocalNoLinksNoLeftovers(t *testing.T) {
	dir := t.TempDir()
	l := &Local{Dir: filepath.Join(dir, "media"), Free: func(string) (uint64, uint64, error) { return 50 << 30, 100 << 30, nil }}
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "publisher.db")
	if err := os.WriteFile(secret, []byte("\x89PNG\r\n\x1a\nsecret"), 0o600); err != nil {
		t.Fatal(err)
	}
	name := strings.Repeat("a", 64) + ".png"
	if err := os.Symlink(secret, filepath.Join(l.Dir, name)); err != nil {
		t.Fatal(err)
	}
	if f, _, _, err := l.Open(name); err == nil {
		f.Close()
		t.Fatal("a link was followed")
	}
	if l.Has(name) {
		t.Fatal("a link counts as stored")
	}
	if err := os.WriteFile(filepath.Join(l.Dir, ".upload-123"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := l.RemoveLeftovers(); err != nil || n != 1 {
		t.Fatalf("leftovers: %d, %v", n, err)
	}
	if _, err := os.Stat(filepath.Join(l.Dir, ".upload-123")); !os.IsNotExist(err) {
		t.Fatal("a leftover kept")
	}
	if n, err := (&Local{Dir: filepath.Join(dir, "none")}).RemoveLeftovers(); err != nil || n != 0 {
		t.Fatalf("no directory: %d, %v", n, err)
	}
}

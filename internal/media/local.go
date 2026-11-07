package media

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Local keeps media on this server's disk (spec §7.6): each object is a
// file named by its sha256 and its type's extension, in Dir (0700, files
// 0600), served at /media/<sha256>.<ext> without sign-in, like a Blossom
// server (the names are content hashes). Its addresses are on BaseURL
// (PUBLIC_URL), or relative when that is unset.
type Local struct {
	Dir string
	// BaseURL is the origin the server is reached at ("" : /media/…).
	BaseURL string
	// Quota is the most bytes the media may take (0: no limit but the
	// free-space floor); read at each upload.
	Quota func() int64
	// IsPublic says whether media.public is on (read at each call).
	IsPublic func() bool
	// Free reports the volume's free and total bytes (nil: statfs).
	Free func(dir string) (free, total uint64, err error)

	mu sync.Mutex // one write at a time: the quota and floor hold
}

// ErrNoSpace: storing the upload would pass the quota or the free-space
// floor (a *NoSpaceError says which).
var ErrNoSpace = errors.New("not enough space for media on this server's disk")

// NoSpaceError is ErrNoSpace with the limit it would pass, in the owner's
// terms.
type NoSpaceError struct{ Reason string }

func (e *NoSpaceError) Error() string   { return ErrNoSpace.Error() + ": " + e.Reason }
func (e *NoSpaceError) Is(t error) bool { return t == ErrNoSpace }

// floorBytes is the free space uploads always leave: 1 GiB or 5 % of the
// volume, whichever is more (the database may share the volume).
func floorBytes(total uint64) uint64 { return max(1<<30, total/20) }

// localTypes are the types served as themselves, by extension: images and
// videos the pipeline makes, as sniffed from the bytes. Anything else is
// stored as .bin and served as an attachment.
var localTypes = map[string]string{
	"jpg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp",
	"mp4": "video/mp4", "webm": "video/webm",
}

func extFor(mime string) string {
	for ext, t := range localTypes {
		if t == mime {
			return ext
		}
	}
	return "bin"
}

var shaHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// localName is a stored object's name: its sha256 and extension.
var localName = regexp.MustCompile(`^([0-9a-f]{64})\.([a-z0-9]{2,5})$`)

// TypeOf is the type a name is served as, and whether it is one
// (attachment: not an image or video, sent to be saved, never shown).
func TypeOf(name string) (mime string, attachment, ok bool) {
	m := localName.FindStringSubmatch(name)
	if m == nil {
		return "", false, false
	}
	if t, known := localTypes[m[2]]; known {
		return t, false, true
	}
	if m[2] == "bin" {
		return "application/octet-stream", true, true
	}
	return "", false, false
}

// Public: media.public is on (it was checked reachable when turned on)
// and the addresses are whole (PUBLIC_URL is set).
func (l *Local) Public() bool { return l.BaseURL != "" && l.IsPublic != nil && l.IsPublic() }

// Describe is where the media go, for the log.
func (l *Local) Describe() string {
	pub := "not public"
	if l.Public() {
		pub = "public"
	}
	return "this server's disk at " + l.Dir + " (" + pub + ")"
}

// URL is the address a stored name is served at.
func (l *Local) URL(name string) string {
	return strings.TrimRight(l.BaseURL, "/") + "/media/" + name
}

// Owns gives the stored name of an address this store gave (with BaseURL
// or relative), or false.
func (l *Local) Owns(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	if u.Scheme != "" || u.Host != "" {
		base, err := url.Parse(l.BaseURL)
		if l.BaseURL == "" || err != nil || u.Scheme != base.Scheme || !strings.EqualFold(u.Host, base.Host) {
			return "", false
		}
	}
	name, ok := strings.CutPrefix(u.Path, "/media/")
	if !ok {
		return "", false
	}
	if _, _, known := TypeOf(name); !known {
		return "", false
	}
	return name, true
}

// Open opens a stored object by a name TypeOf takes (the path is made
// from the validated name only).
func (l *Local) Open(name string) (*os.File, string, bool, error) {
	mime, attach, ok := TypeOf(name)
	if !ok {
		return nil, "", false, fs.ErrNotExist
	}
	f, err := os.Open(filepath.Join(l.Dir, name))
	return f, mime, attach, err
}

// Has: a stored object by that name is on disk.
func (l *Local) Has(name string) bool {
	if _, _, ok := TypeOf(name); !ok {
		return false
	}
	fi, err := os.Stat(filepath.Join(l.Dir, name))
	return err == nil && fi.Mode().IsRegular()
}

// Reachable: an address of this store that a network fetching media by
// address can reach (served publicly, and whole: given with PUBLIC_URL).
func (l *Local) Reachable(raw string) bool {
	_, mine := l.Owns(raw)
	return mine && l.Public() && !strings.HasPrefix(raw, "/")
}

// Fetch reads a stored object by its address, from disk (never over
// HTTP), at most FetchCap bytes.
func (l *Local) Fetch(_ context.Context, raw string) ([]byte, string, error) {
	name, ok := l.Owns(raw)
	if !ok {
		return nil, "", fmt.Errorf("not an address of this server's media")
	}
	f, mime, _, err := l.Open(name)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, fetchCapBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(b)) > fetchCapBytes {
		return nil, "", fmt.Errorf("media %s exceeds %d-byte cap", name, fetchCapBytes)
	}
	return b, mime, nil
}

// Upload stores body under its sha256 (checked as written) with the
// extension of the type its bytes are, and returns its address.
func (l *Local) Upload(_ context.Context, body io.Reader, size int64, sha, mime string) (string, error) {
	if !shaHex.MatchString(sha) {
		return "", errors.New("media: not a sha256")
	}
	head := make([]byte, 512)
	n, err := io.ReadFull(body, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return "", err
	}
	head = head[:n]
	// The bytes say what they are; the type given is believed only when
	// they agree (an HTML page named image/png is stored as .bin).
	ext := "bin"
	if sniffed := sniff(head); extFor(sniffed) != "bin" && (sniffed == mime || mime == "" || mime == "application/octet-stream") {
		ext = extFor(sniffed)
	}
	name := sha + "." + ext
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := os.MkdirAll(l.Dir, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(l.Dir, name)); err == nil {
		return l.URL(name), nil // stored already: content-addressed
	}
	if err := l.room(size); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(l.Dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	h := sha256.New()
	w := io.MultiWriter(tmp, h)
	if _, err := w.Write(head); err != nil {
		tmp.Close()
		return "", err
	}
	if _, err := io.Copy(w, body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != sha {
		return "", fmt.Errorf("media: the bytes changed while stored (sha256 %s, want %s)", got, sha)
	}
	if err := os.Rename(tmp.Name(), filepath.Join(l.Dir, name)); err != nil {
		return "", err
	}
	return l.URL(name), nil
}

// sniff is the type the bytes are, as the pipeline's types go (an ISO
// media file by its brand, WebM by its EBML header).
func sniff(head []byte) string {
	if len(head) >= 12 && string(head[4:8]) == "ftyp" {
		switch string(head[8:12]) {
		case "qt  ":
			return "video/quicktime"
		case "heic", "heix", "mif1", "msf1":
			return "image/heic"
		case "avif":
			return "image/avif"
		}
		return "video/mp4"
	}
	if bytes.HasPrefix(head, []byte{0x1a, 0x45, 0xdf, 0xa3}) {
		return "video/webm"
	}
	t, _, _ := strings.Cut(http.DetectContentType(head), ";")
	return t
}

// room checks that size more bytes fit the quota and leave the floor
// free (l.mu held).
func (l *Local) room(size int64) error {
	if size < 0 {
		size = 0
	}
	if l.Quota != nil {
		if q := l.Quota(); q > 0 {
			used, err := l.used()
			if err != nil {
				return err
			}
			if used+size > q {
				return &NoSpaceError{Reason: fmt.Sprintf("the media would take %d MiB, past the %d MiB set in Settings, Media",
					mib(used+size), q>>20)}
			}
		}
	}
	free := l.Free
	if free == nil {
		free = statfs
	}
	f, total, err := free(l.Dir)
	if err != nil {
		return err
	}
	if floor := floorBytes(total); f < uint64(size)+floor {
		return &NoSpaceError{Reason: fmt.Sprintf("uploads always leave %d MiB of the disk free", floor>>20)}
	}
	return nil
}

// used is the bytes the stored media take.
func (l *Local) used() (int64, error) {
	entries, err := os.ReadDir(l.Dir)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, e := range entries {
		if _, _, ok := TypeOf(e.Name()); !ok {
			continue
		}
		if info, err := e.Info(); err == nil {
			n += info.Size()
		}
	}
	return n, nil
}

// mib is n bytes in MiB, rounded up.
func mib(n int64) int64 { return (n + 1<<20 - 1) >> 20 }

func statfs(dir string) (free, total uint64, err error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(dir, &s); err != nil {
		return 0, 0, err
	}
	return s.Bavail * uint64(s.Bsize), s.Blocks * uint64(s.Bsize), nil
}

// Remove deletes a stored object (the public self-test's probe).
func (l *Local) Remove(name string) error {
	if _, _, ok := TypeOf(name); !ok {
		return fs.ErrNotExist
	}
	return os.Remove(filepath.Join(l.Dir, name))
}

// LocalFirst fetches this server's disk media from disk, and anything
// else with Next.
type LocalFirst struct {
	Local *Local
	Next  interface {
		Fetch(ctx context.Context, url string) ([]byte, string, error)
	}
}

// Fetch implements the dispatcher's Fetcher.
func (f LocalFirst) Fetch(ctx context.Context, raw string) ([]byte, string, error) {
	if f.Local != nil {
		if _, ok := f.Local.Owns(raw); ok {
			return f.Local.Fetch(ctx, raw)
		}
	}
	return f.Next.Fetch(ctx, raw)
}

// ErrNoPublicURL: media can't be served publicly without PUBLIC_URL.
var ErrNoPublicURL = errors.New("set PUBLIC_URL first: media are served publicly at PUBLIC_URL/media/")

// ProbeError says why this server's media aren't reachable at its public
// address: what it answered (a status, a redirect, other bytes).
type ProbeError struct {
	URL      string
	Status   int
	Redirect bool
	Mismatch bool
	// Store: this server's disk didn't take the probe (nothing was
	// fetched).
	Store error
}

func (e *ProbeError) Unwrap() error { return e.Store }

func (e *ProbeError) Error() string {
	switch {
	case e.Store != nil:
		return "this server's disk didn't take the self-test's file: " + e.Store.Error()
	case e.Redirect:
		return e.URL + " answered with a redirect (" + fmt.Sprint(e.Status) + "): a sign-in in front of /media/ must let it through"
	case e.Mismatch:
		return e.URL + " answered with something other than the file: is /media/ sent to this server?"
	case e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden:
		return e.URL + " answered " + fmt.Sprint(e.Status) + ": a sign-in in front of /media/ must let it through"
	}
	return e.URL + " answered " + fmt.Sprint(e.Status)
}

// CheckPublic checks that this server's media are reachable at BaseURL,
// as a network fetching them would reach them: a probe is stored, fetched
// from BaseURL/media/ (hc follows no redirects) and must come back whole
// with a 200; it is removed after. An error is ErrNoPublicURL, a
// *ProbeError, or the transport's (unreachable, a timeout).
func (l *Local) CheckPublic(ctx context.Context, hc *http.Client) error {
	if l.BaseURL == "" {
		return ErrNoPublicURL
	}
	probe, err := newProbe()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(probe)
	sha := hex.EncodeToString(sum[:])
	u, err := l.Upload(ctx, bytes.NewReader(probe), int64(len(probe)), sha, "application/octet-stream")
	if err != nil {
		return &ProbeError{Store: err}
	}
	defer func() { _ = l.Remove(sha + ".bin") }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	pe := &ProbeError{URL: l.URL("…"), Status: resp.StatusCode}
	switch {
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		pe.Redirect = true
		return pe
	case resp.StatusCode != http.StatusOK:
		return pe
	}
	got, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(probe))+1))
	if err != nil {
		return err
	}
	if !bytes.Equal(got, probe) {
		pe.Mismatch = true
		return pe
	}
	return nil
}

// newProbe is a self-test's object: random, so removing it after never
// removes an upload.
func newProbe() ([]byte, error) {
	r := make([]byte, 32)
	if _, err := rand.Read(r); err != nil {
		return nil, err
	}
	return []byte("publisher media probe " + hex.EncodeToString(r)), nil
}

// NoRedirects is a client for CheckPublic: it follows no redirect (one is
// a proxy's sign-in page) and gives up after 10 seconds.
func NoRedirects() *http.Client {
	return &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// CheckWritable stores a probe and reads it back from disk.
func (l *Local) CheckWritable(ctx context.Context) error {
	probe, err := newProbe()
	if err != nil {
		return err
	}
	sum := sha256.Sum256(probe)
	sha := hex.EncodeToString(sum[:])
	u, err := l.Upload(ctx, bytes.NewReader(probe), int64(len(probe)), sha, "application/octet-stream")
	if err != nil {
		return err
	}
	defer func() { _ = l.Remove(sha + ".bin") }()
	got, _, err := l.Fetch(ctx, u)
	if err == nil && !bytes.Equal(got, probe) {
		err = errors.New("the probe read back differs")
	}
	return err
}

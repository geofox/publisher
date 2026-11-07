package media

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/geofox/publisher/internal/redact"
)

// Store keeps processed media and says where they are (spec §7.6). The
// pipeline strips metadata and hashes before it calls one.
type Store interface {
	// Upload stores body (size bytes; sha is its sha256, hex) and returns
	// the address it is served at.
	Upload(ctx context.Context, body io.Reader, size int64, sha, mime string) (string, error)
	// Public: services that fetch media by address (Threads, Nostr) can
	// reach what it serves.
	Public() bool
}

// ErrNoStore: no media store is set (Settings, Media: "Nowhere").
var ErrNoStore = errors.New("no media store is set: choose one in Settings, Media")

// UnusableError: a media store is set but can't be used now; Reason says
// why in the owner's terms (no signing key, no address).
type UnusableError struct{ Reason string }

func (e *UnusableError) Error() string { return "the media store can't be used: " + e.Reason }

// Blossom is a Blossom server (BUD-02 PUT /upload), each upload
// authorized by a signed Nostr event (kind 24242, BUD-11).
type Blossom struct {
	// URL is the server's address, without a trailing slash.
	URL string
	Key nostr.SecretKey
	// Own: Key is the store's own (media.blossom_key), so the
	// authorization names this server (BUD-11's server tag: one caught on
	// the way can't be used on another), and the address the server
	// answers must be its own. The environment's Nostr key signs as it
	// always has, without the tag, for servers set before.
	Own bool
	// Confined: the address the server answers must be on its own origin,
	// else its own address for the hash, so a server set in the app can't
	// point posts, and the fetches that send them again, at another host.
	// A server with the store's own key is always confined.
	Confined bool
	// HTTP carries the uploads (no overall timeout: a video is large).
	HTTP *http.Client
}

// Public: a Blossom server serves anyone.
func (b *Blossom) Public() bool { return true }

// Describe is the server and who signs for it, for the log.
func (b *Blossom) Describe() string {
	who := "the environment's Nostr key"
	if b.Own {
		who = "its own key"
	}
	return "Blossom server " + b.URL + ", signed with " + who + " (" + nip19.EncodeNpub(nostr.GetPublicKey(b.Key)) + ")"
}

// Base is the server's address.
func (b *Blossom) Base() string { return b.URL }

// Upload PUTs body to the server and returns the address it answers (or
// URL/sha when it answers none).
func (b *Blossom) Upload(ctx context.Context, body io.Reader, size int64, sha, mime string) (string, error) {
	base, err := url.Parse(b.URL)
	if err != nil || base.Host == "" {
		return "", errors.New("the Blossom server's address isn't one")
	}
	auth, err := b.authorization(sha)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, b.URL+"/upload", body)
	if err != nil {
		return "", redact.URLError(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", mime)
	req.Header.Set("X-SHA-256", sha) // BUD-02: the hash the authorization names
	req.ContentLength = size
	// net/http sets GetBody for bytes and strings readers but not files; a
	// seekable body restores stale-keep-alive retries and 307/308
	// redirects for the streaming path.
	if req.GetBody == nil {
		if s, ok := body.(io.Seeker); ok {
			req.GetBody = func() (io.ReadCloser, error) {
				if _, err := s.Seek(0, io.SeekStart); err != nil {
					return nil, err
				}
				return io.NopCloser(body), nil
			}
		}
	}
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return "", redact.URLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return "", fmt.Errorf("blossom %d: %s", resp.StatusCode, redact.Upstream(string(rb)))
	}
	var bd struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&bd); err != nil {
		return "", fmt.Errorf("decode blossom: %w", err)
	}
	if bd.URL == "" {
		return b.URL + "/" + sha, nil
	}
	if b.Own || b.Confined {
		return ownBlobURL(base, bd.URL, sha), nil
	}
	return bd.URL, nil
}

// authorization is the Authorization header for uploading sha: a signed
// kind 24242 event (BUD-11) good for five minutes.
func (b *Blossom) authorization(sha string) (string, error) {
	base, err := url.Parse(b.URL)
	if err != nil || base.Host == "" {
		return "", errors.New("the Blossom server's address isn't one")
	}
	now := time.Now()
	tags := nostr.Tags{
		{"t", "upload"}, {"x", sha},
		{"expiration", strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10)},
	}
	if b.Own {
		// "The value MUST be a lowercase domain name only" (BUD-11).
		tags = append(tags, nostr.Tag{"server", strings.ToLower(base.Hostname())})
	}
	ev := nostr.Event{PubKey: nostr.GetPublicKey(b.Key), CreatedAt: nostr.Timestamp(now.Unix()), Kind: 24242,
		Tags: tags, Content: "Upload"}
	if err := ev.Sign(b.Key); err != nil {
		return "", fmt.Errorf("sign auth: %w", err)
	}
	j, err := json.Marshal(ev)
	if err != nil {
		return "", fmt.Errorf("marshal auth: %w", err)
	}
	// Standard base64, as servers set up before BUD-11 read it (BUD-11
	// asks for base64url without padding).
	return "Nostr " + base64.StdEncoding.EncodeToString(j), nil
}

var blobExt = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

// ownBlobURL is where a server set in the app serves a blob: the address
// it answered when that is on its own origin, else its own address for
// the hash (BUD-01: GET /<sha256>[.ext]), so a server can't point posts
// (and the fetches that retry them) at another host.
func ownBlobURL(base *url.URL, answered, sha string) string {
	u, err := url.Parse(answered)
	if err == nil && u.Scheme == base.Scheme && strings.EqualFold(u.Host, base.Host) && u.User == nil {
		return u.String()
	}
	ext := ""
	if err == nil {
		if e := strings.ToLower(path.Ext(u.Path)); blobExt.MatchString(e) && strings.EqualFold(path.Base(u.Path), sha+e) {
			ext = e
		}
	}
	return strings.TrimRight(base.String(), "/") + "/" + sha + ext
}

// Check asks the server whether it would take an upload signed with the
// key (BUD-06 HEAD /upload) and returns its status; a server without
// BUD-06 answers 404 or 405, which says only that it is reachable.
func (b *Blossom) Check(ctx context.Context) (int, error) {
	probe := []byte("publisher upload check")
	sum := sha256.Sum256(probe)
	sha := hex.EncodeToString(sum[:])
	auth, err := b.authorization(sha)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, b.URL+"/upload", nil)
	if err != nil {
		return 0, redact.URLError(err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-SHA-256", sha)
	req.Header.Set("X-Content-Length", strconv.Itoa(len(probe)))
	req.Header.Set("X-Content-Type", "application/octet-stream")
	resp, err := b.HTTP.Do(req)
	if err != nil {
		return 0, redact.URLError(err)
	}
	resp.Body.Close()
	return resp.StatusCode, nil
}

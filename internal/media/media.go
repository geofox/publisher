package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/buckket/go-blurhash"
	"github.com/geofox/publisher/internal/transcode"
	_ "golang.org/x/image/webp"
)

// Result is the processed-media record: a Blossom URL, content hashes, and the
// raw bytes (kept so platform clients that need a binary upload — Bluesky,
// Mastodon — don't re-download).
type Result struct {
	URL          string    `json:"url"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	Mime         string    `json:"mime"`
	Dim          string    `json:"dim,omitempty"`
	Blurhash     string    `json:"blurhash,omitempty"`
	DurationSecs int64     `json:"duration_secs,omitempty"`
	PosterURL    string    `json:"poster_url,omitempty"` // poster JPEG URL (videos; set by the videojob)
	Imeta        nostr.Tag `json:"imeta"`
	Bytes        []byte    `json:"-"`
}

type Pipeline struct {
	BlossomURL string
	NSEC       nostr.SecretKey
	OwnerPub   nostr.PubKey
	HTTP       *http.Client
	// StreamHTTP carries large streaming PUTs (video canonicals): no overall
	// Client.Timeout (a 1 GB body cannot fit a fixed budget) — cancellation
	// comes from the caller's ctx, and the transport bounds the hang-prone
	// phases (dial, TLS, response-header wait) individually.
	StreamHTTP *http.Client
	Lookup     func(sha256 string) (string, bool, error) // optional; if set, returning ok=true short-circuits the Blossom upload
}

func New(blossomURL string, nsec nostr.SecretKey, pub nostr.PubKey) *Pipeline {
	return &Pipeline{
		BlossomURL: strings.TrimRight(blossomURL, "/"),
		NSEC:       nsec, OwnerPub: pub,
		HTTP: &http.Client{Timeout: 30 * time.Second},
		StreamHTTP: &http.Client{Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
		}},
	}
}

// Process hashes the bytes, extracts image metadata, uploads to Blossom (always),
// and returns the full Result. mime falls back to application/octet-stream.
func (p *Pipeline) Process(ctx context.Context, body []byte, mime string) (Result, error) {
	// HEIC never becomes a canonical object: convert before hashing so the
	// stored sha256/dim/blurhash/imeta describe the JPEG that platforms and
	// browsers actually consume. Belt-and-braces with the composer's
	// attach-time conversion — this also covers token-API /upload-media
	// clients. A conversion that cannot produce JPEG (corrupt input passes
	// through transcode.Image unchanged) is rejected outright.
	if transcode.IsHEIC(mime, body) {
		conv, cerr := convertHEIC(body, mime)
		if cerr != nil {
			return Result{}, fmt.Errorf("heic convert: %w", cerr)
		}
		body, mime = conv.Bytes, conv.Mime
	}
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if mime == "" {
		mime = "application/octet-stream"
	}
	var dim, bh string
	if strings.HasPrefix(mime, "image/") {
		if d, h, err := extractImageMeta(body); err == nil {
			dim, bh = d, h
		}
	}
	if p.Lookup != nil {
		if existing, ok, err := p.Lookup(sha); err != nil {
			return Result{}, fmt.Errorf("media: lookup sha256: %w", err)
		} else if ok {
			return Result{URL: existing, SHA256: sha, Size: int64(len(body)), Mime: mime,
				Dim: dim, Blurhash: bh, Imeta: ImetaTag(existing, mime, sha, dim, bh, ""), Bytes: body}, nil
		}
	}

	url, err := p.blossomUpload(ctx, body, sha, mime)
	if err != nil {
		return Result{}, fmt.Errorf("blossom upload: %w", err)
	}
	return Result{URL: url, SHA256: sha, Size: int64(len(body)), Mime: mime,
		Dim: dim, Blurhash: bh, Imeta: ImetaTag(url, mime, sha, dim, bh, ""), Bytes: body}, nil
}

// ImetaTag assembles a NIP-92 imeta tag from media attributes. It is the single
// authority on the tag layout so every code path that embeds media in a Nostr
// event (the upload pipeline and the cross-post dispatcher) stays byte-identical.
// dim, blurhash, and image are optional and omitted when empty. image is the
// preview-image URL (NIP-92 "image" field) used for video poster frames.
func ImetaTag(url, mime, sha, dim, blurhash, image string) nostr.Tag {
	t := nostr.Tag{"imeta", "url " + url, "m " + mime, "x " + sha}
	if dim != "" {
		t = append(t, "dim "+dim)
	}
	if blurhash != "" {
		t = append(t, "blurhash "+blurhash)
	}
	if image != "" {
		t = append(t, "image "+image)
	}
	return t
}

func (p *Pipeline) blossomUpload(ctx context.Context, body []byte, sha, mime string) (string, error) {
	return p.blossomUploadStream(ctx, bytes.NewReader(body), int64(len(body)), sha, mime)
}

// blossomUploadStream is blossomUpload with a streaming body (the auth event
// signs the sha, not the bytes, so streaming changes nothing semantically).
func (p *Pipeline) blossomUploadStream(ctx context.Context, body io.Reader, size int64, sha, mime string) (string, error) {
	auth := nostr.Event{
		PubKey: p.OwnerPub, CreatedAt: nostr.Timestamp(time.Now().Unix()), Kind: 24242,
		Tags: nostr.Tags{
			{"t", "upload"}, {"x", sha},
			{"expiration", strconv.FormatInt(time.Now().Add(5*time.Minute).Unix(), 10)},
		},
		Content: "Upload",
	}
	if err := auth.Sign(p.NSEC); err != nil {
		return "", fmt.Errorf("sign auth: %w", err)
	}
	authJSON, err := json.Marshal(auth)
	if err != nil {
		return "", fmt.Errorf("marshal auth: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.BlossomURL+"/upload", body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(authJSON))
	req.Header.Set("Content-Type", mime)
	req.ContentLength = size
	// net/http auto-sets GetBody for bytes/strings readers but not files; a
	// seekable body restores stale-keep-alive retries and 307/308 redirects
	// for the streaming path (parity with the old bytes path).
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
	cl := p.StreamHTTP
	if cl == nil {
		cl = p.HTTP
	}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		rb, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("blossom %d: %s", resp.StatusCode, string(rb))
	}
	var bd struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&bd); err != nil {
		return "", fmt.Errorf("decode blossom: %w", err)
	}
	if bd.URL == "" {
		return p.BlossomURL + "/" + sha, nil
	}
	return bd.URL, nil
}

// FetchCap bounds re-pulled media in RAM. Raised from 64 MB for video: the
// dispatch fetch policy only pulls video bytes when a byte-upload platform's
// gate (≤100 MB) can use them, so 110 MB covers the worst legitimate case.
const FetchCap = 110 << 20

// fetchCapBytes is the runtime cap used by Fetch. It matches FetchCap but is
// an unexported variable so tests can temporarily lower it as a seam without
// touching the public constant.
var fetchCapBytes int64 = FetchCap

// Fetch GETs the bytes at url (used to re-pull archived media from Blossom for
// retries). Returns the body and Content-Type. Errors beyond fetchCapBytes
// rather than silently truncating.
func (p *Pipeline) Fetch(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("fetch %s: %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchCapBytes+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > fetchCapBytes {
		return nil, "", fmt.Errorf("fetch %s: exceeds %d-byte cap", url, fetchCapBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// ProcessFile is the streaming sibling of Process for large media (video):
// sha256 and the Blossom PUT both stream from disk so a 1 GB canonical never
// occupies RAM. No blurhash for video; dim and duration come from the caller's
// probe. progress (nil ok) receives upload fractions. Result.Bytes stays nil —
// consumers fetch by URL under the dispatch fetch policy.
func (p *Pipeline) ProcessFile(ctx context.Context, path, mime, dim string, durationSecs int64, progress func(float64)) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return Result{}, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return Result{}, fmt.Errorf("hash: %w", err)
	}
	sha := hex.EncodeToString(h.Sum(nil))
	if mime == "" {
		mime = "application/octet-stream"
	}
	mk := func(url string) Result {
		return Result{URL: url, SHA256: sha, Size: st.Size(), Mime: mime, Dim: dim,
			DurationSecs: durationSecs, Imeta: ImetaTag(url, mime, sha, dim, "", "")}
	}
	if p.Lookup != nil {
		if existing, ok, lerr := p.Lookup(sha); lerr != nil {
			return Result{}, fmt.Errorf("media: lookup sha256: %w", lerr)
		} else if ok {
			return mk(existing), nil
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return Result{}, err
	}
	body := io.Reader(f)
	if progress != nil {
		body = &countingReader{r: f, total: st.Size(), cb: progress}
	}
	url, err := p.blossomUploadStream(ctx, body, st.Size(), sha, mime)
	if err != nil {
		return Result{}, fmt.Errorf("blossom upload: %w", err)
	}
	return mk(url), nil
}

type countingReader struct {
	r     io.Reader
	total int64
	read  int64
	cb    func(float64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.total > 0 {
		c.cb(float64(c.read) / float64(c.total))
	}
	return n, err
}

func (c *countingReader) Seek(offset int64, whence int) (int64, error) {
	s, ok := c.r.(io.Seeker)
	if !ok {
		return 0, fmt.Errorf("countingReader: underlying reader not seekable")
	}
	n, err := s.Seek(offset, whence)
	if err == nil && offset == 0 && whence == io.SeekStart {
		c.read = 0
	}
	return n, err
}

// maxImagePixels caps the decoded pixel count to defuse decompression bombs: a
// few-KB image header can declare gigapixel dimensions that blow up RAM on a
// full Decode. ~100 MP covers any real-world photo while bounding the bitmap.
const maxImagePixels = 100_000_000

// convertHEIC re-encodes a HEIC upload as full-size JPEG (the composer's
// "convert" preset), so the rest of the pipeline only ever sees web formats.
// transcode.Image passes undecodable input through unchanged rather than
// erroring — map that to an explicit error here.
func convertHEIC(body []byte, mime string) (transcode.Result, error) {
	p, ok := transcode.PresetParams("convert")
	if !ok {
		panic("transcode: convert preset missing") // same-repo constant; drift is a programmer error
	}
	r, err := transcode.Image(body, mime, p)
	if err != nil {
		return transcode.Result{}, err
	}
	if !r.Changed || r.Mime != "image/jpeg" {
		return transcode.Result{}, fmt.Errorf("undecodable heic (%d bytes)", len(body))
	}
	return r, nil
}

func extractImageMeta(buf []byte) (dim, bh string, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxImagePixels {
		return "", "", fmt.Errorf("image %dx%d exceeds %d-pixel cap", cfg.Width, cfg.Height, maxImagePixels)
	}
	img, _, err := image.Decode(bytes.NewReader(buf))
	if err != nil {
		return "", "", err
	}
	b := img.Bounds()
	dim = fmt.Sprintf("%dx%d", b.Dx(), b.Dy())
	hash, err := blurhash.Encode(4, 4, img)
	if err != nil {
		return dim, "", err
	}
	return dim, hash, nil
}

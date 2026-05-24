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
	"net/http"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/buckket/go-blurhash"
	_ "golang.org/x/image/webp"
)

// Result is the processed-media record: a Blossom URL, content hashes, and the
// raw bytes (kept so platform clients that need a binary upload — Bluesky,
// Mastodon — don't re-download).
type Result struct {
	URL      string    `json:"url"`
	SHA256   string    `json:"sha256"`
	Size     int64     `json:"size"`
	Mime     string    `json:"mime"`
	Dim      string    `json:"dim,omitempty"`
	Blurhash string    `json:"blurhash,omitempty"`
	Imeta    nostr.Tag `json:"imeta"`
	Bytes    []byte    `json:"-"`
}

type Pipeline struct {
	BlossomURL string
	NSEC       nostr.SecretKey
	OwnerPub   nostr.PubKey
	HTTP       *http.Client
}

func New(blossomURL string, nsec nostr.SecretKey, pub nostr.PubKey) *Pipeline {
	return &Pipeline{
		BlossomURL: strings.TrimRight(blossomURL, "/"),
		NSEC:       nsec, OwnerPub: pub,
		HTTP: &http.Client{Timeout: 30 * time.Second},
	}
}

// Process hashes the bytes, extracts image metadata, uploads to Blossom (always),
// and returns the full Result. mime falls back to application/octet-stream.
func (p *Pipeline) Process(ctx context.Context, body []byte, mime string) (Result, error) {
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
	url, err := p.blossomUpload(ctx, body, sha, mime)
	if err != nil {
		return Result{}, fmt.Errorf("blossom upload: %w", err)
	}
	return Result{URL: url, SHA256: sha, Size: int64(len(body)), Mime: mime,
		Dim: dim, Blurhash: bh, Imeta: ImetaTag(url, mime, sha, dim, bh), Bytes: body}, nil
}

// ImetaTag assembles a NIP-92 imeta tag from media attributes. It is the single
// authority on the tag layout so every code path that embeds media in a Nostr
// event (the upload pipeline and the cross-post dispatcher) stays byte-identical.
// dim and blurhash are optional and omitted when empty.
func ImetaTag(url, mime, sha, dim, blurhash string) nostr.Tag {
	t := nostr.Tag{"imeta", "url " + url, "m " + mime, "x " + sha}
	if dim != "" {
		t = append(t, "dim "+dim)
	}
	if blurhash != "" {
		t = append(t, "blurhash "+blurhash)
	}
	return t
}

func (p *Pipeline) blossomUpload(ctx context.Context, body []byte, sha, mime string) (string, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, p.BlossomURL+"/upload", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Nostr "+base64.StdEncoding.EncodeToString(authJSON))
	req.Header.Set("Content-Type", mime)
	req.ContentLength = int64(len(body))
	resp, err := p.HTTP.Do(req)
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

// Fetch GETs the bytes at url (used to re-pull archived media from Blossom for
// retries). Returns the body and Content-Type. Capped at 64 MB.
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// maxImagePixels caps the decoded pixel count to defuse decompression bombs: a
// few-KB image header can declare gigapixel dimensions that blow up RAM on a
// full Decode. ~100 MP covers any real-world photo while bounding the bitmap.
const maxImagePixels = 100_000_000

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

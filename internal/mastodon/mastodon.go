package mastodon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	gomast "github.com/mattn/go-mastodon"

	"github.com/geofox/publisher/internal/destination"
)

// Image is one attachment. Mastodon detects the content type from the
// bytes server-side, so no MIME field is needed here.
type Image struct {
	Bytes []byte
	Alt   string
}

// Video is one attached video; Mastodon transcodes server-side after an
// async-processing window.
type Video struct {
	Bytes []byte
	Alt   string
}

type Post struct {
	Text        string
	SpoilerText string
	Sensitive   bool
	Visibility  string // public|unlisted|private|direct ("" → instance default)
	Language    string
	Images      []Image
	Video       *Video
	InReplyToID string // when set, posts as a reply (threading)
	// IdempotencyKey, when set, is sent as the Idempotency-Key header on the
	// status create, so the server drops a repeat of the same post (Mastodon
	// keeps keys for up to an hour).
	IdempotencyKey string
}

type Result struct {
	RemoteID  string
	RemoteURL string
}

type Client struct {
	c                *gomast.Client
	baseURL          string
	token            string
	http             *http.Client
	pollInterval     time.Duration
	videoPollTimeout time.Duration
}

func New(baseURL, token string) *Client {
	return NewWithNet(baseURL, token, destination.TrustedNet())
}

// NewWithNet is New with every connection made through n.
func NewWithNet(baseURL, token string, n destination.Net) *Client {
	gc := gomast.NewClient(&gomast.Config{Server: baseURL, AccessToken: token})
	// go-mastodon copies http.DefaultClient: no timeout, and it retries a
	// 429 itself with sleeps of up to an hour while holding the worker.
	// Bound the hang-prone phases (a server that accepts and never answers)
	// by the dial, handshake and answer timeouts, not an overall one: a
	// large video on a slow uplink takes longer than any fixed ceiling (the
	// caller's context still bounds the whole). Hand a 429
	// back as an error so the engine sees the rate limit and its reset time.
	gc.Client = http.Client{
		Transport: rateLimitTransport{base: idempotencyTransport{base: n.Transport(destination.TransportOptions{
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		})}},
		CheckRedirect: n.CheckRedirect,
	}
	return &Client{
		c:                gc,
		baseURL:          strings.TrimRight(baseURL, "/"),
		token:            token,
		http:             n.Client(15 * time.Second),
		pollInterval:     2 * time.Second,
		videoPollTimeout: 5 * time.Minute,
	}
}

func (cl *Client) Post(ctx context.Context, p Post) (Result, error) {
	ctx = WithIdempotencyKey(ctx, p.IdempotencyKey)
	var mediaIDs []gomast.ID

	if p.Video != nil {
		att, err := cl.c.UploadMediaFromMedia(ctx, &gomast.Media{
			File: bytes.NewReader(p.Video.Bytes), Description: p.Video.Alt,
		})
		if err != nil {
			return Result{}, fmt.Errorf("video upload: %w", err)
		}
		if err := cl.waitMediaReady(ctx, string(att.ID)); err != nil {
			return Result{}, err
		}
		mediaIDs = append(mediaIDs, att.ID)
	}

	for _, img := range p.Images {
		att, err := cl.c.UploadMediaFromMedia(ctx, &gomast.Media{
			File: bytes.NewReader(img.Bytes), Description: img.Alt,
		})
		if err != nil {
			return Result{}, err
		}
		mediaIDs = append(mediaIDs, att.ID)
	}
	if err := ctx.Err(); err != nil { // nothing sent
		return Result{}, err
	}
	if err := destination.BeforeCommit(ctx); err != nil { // nothing sent
		return Result{}, err
	}
	st, err := cl.c.PostStatus(ctx, &gomast.Toot{
		Status: p.Text, SpoilerText: p.SpoilerText, Sensitive: p.Sensitive,
		Visibility: p.Visibility, Language: p.Language, MediaIDs: mediaIDs,
		InReplyToID: gomast.ID(p.InReplyToID),
	})
	if err != nil {
		return Result{}, destination.Committing(err)
	}
	return Result{RemoteID: string(st.ID), RemoteURL: st.URL}, nil
}

// waitMediaReady polls GET /api/v1/media/:id until the server returns 200
// (processing complete) or a non-206 error status. 206 = still processing.
func (cl *Client) waitMediaReady(ctx context.Context, id string) error {
	deadline := time.Now().Add(cl.videoPollTimeout)
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cl.baseURL+"/api/v1/media/"+id, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+cl.token)
		resp, err := cl.http.Do(req)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
			return nil
		case http.StatusPartialContent:
			// still processing
		default:
			return fmt.Errorf("media %s processing: status %d", id, resp.StatusCode)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("media %s not processed within %s", id, cl.videoPollTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cl.pollInterval):
		}
	}
}

type idemKey struct{}

// WithIdempotencyKey attaches an Idempotency-Key to status creates made with
// ctx. "" leaves ctx unchanged.
func WithIdempotencyKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, idemKey{}, key)
}

// idempotencyTransport sets the Idempotency-Key header on POST
// /api/v1/statuses when the request context carries one. go-mastodon has no
// hook for extra headers, so it is done at the transport.
type idempotencyTransport struct{ base http.RoundTripper }

func (t idempotencyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if k, ok := req.Context().Value(idemKey{}).(string); ok && req.Method == http.MethodPost &&
		strings.HasSuffix(req.URL.Path, "/api/v1/statuses") && req.Header.Get("Idempotency-Key") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Idempotency-Key", k)
	}
	return t.base.RoundTrip(req)
}

// rateLimitTransport turns a 429 into an error carrying the response
// headers, so go-mastodon doesn't retry it internally and classify sees
// rate_limited with the instance's X-RateLimit-Reset.
type rateLimitTransport struct{ base http.RoundTripper }

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusTooManyRequests {
		return resp, err
	}
	defer resp.Body.Close()
	return nil, destination.NewHTTPError(req.URL.Path, resp)
}

// InstanceLimits are the posting limits an instance announces in
// GET /api/v2/instance (Mastodon 4.0+). Zero fields were not announced.
type InstanceLimits struct {
	MaxCharacters            int
	MaxMediaAttachments      int
	CharactersReservedPerURL int
	ImageSizeLimit           int64
	ImageMatrixLimit         int64
	VideoSizeLimit           int64
	DescriptionLimit         int
}

// Instance fetches the instance's posting limits.
func (cl *Client) Instance(ctx context.Context) (InstanceLimits, error) {
	var v struct {
		Configuration struct {
			Statuses struct {
				MaxCharacters            int `json:"max_characters"`
				MaxMediaAttachments      int `json:"max_media_attachments"`
				CharactersReservedPerURL int `json:"characters_reserved_per_url"`
			} `json:"statuses"`
			MediaAttachments struct {
				ImageSizeLimit   int64 `json:"image_size_limit"`
				ImageMatrixLimit int64 `json:"image_matrix_limit"`
				VideoSizeLimit   int64 `json:"video_size_limit"`
				DescriptionLimit int   `json:"description_limit"`
			} `json:"media_attachments"`
		} `json:"configuration"`
	}
	if err := cl.getJSON(ctx, "/api/v2/instance", nil, &v); err != nil {
		return InstanceLimits{}, err
	}
	st, ma := v.Configuration.Statuses, v.Configuration.MediaAttachments
	return InstanceLimits{
		MaxCharacters: st.MaxCharacters, MaxMediaAttachments: st.MaxMediaAttachments,
		CharactersReservedPerURL: st.CharactersReservedPerURL,
		ImageSizeLimit:           ma.ImageSizeLimit, ImageMatrixLimit: ma.ImageMatrixLimit,
		VideoSizeLimit: ma.VideoSizeLimit, DescriptionLimit: ma.DescriptionLimit,
	}, nil
}

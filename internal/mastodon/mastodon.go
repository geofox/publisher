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
}

type Result struct {
	RemoteID  string
	RemoteURL string
}

type Client struct {
	c            *gomast.Client
	baseURL      string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

func New(baseURL, token string) *Client {
	return &Client{
		c:            gomast.NewClient(&gomast.Config{Server: baseURL, AccessToken: token}),
		baseURL:      strings.TrimRight(baseURL, "/"),
		token:        token,
		http:         &http.Client{Timeout: 15 * time.Second},
		pollInterval: 2 * time.Second,
	}
}

func (cl *Client) Post(ctx context.Context, p Post) (Result, error) {
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
	st, err := cl.c.PostStatus(ctx, &gomast.Toot{
		Status: p.Text, SpoilerText: p.SpoilerText, Sensitive: p.Sensitive,
		Visibility: p.Visibility, Language: p.Language, MediaIDs: mediaIDs,
		InReplyToID: gomast.ID(p.InReplyToID),
	})
	if err != nil {
		return Result{}, err
	}
	return Result{RemoteID: string(st.ID), RemoteURL: st.URL}, nil
}

// waitMediaReady polls GET /api/v1/media/:id until the server returns 200
// (processing complete) or a non-206 error status. 206 = still processing.
func (cl *Client) waitMediaReady(ctx context.Context, id string) error {
	for i := 0; i < 150; i++ {
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
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cl.pollInterval):
		}
	}
	return fmt.Errorf("media %s not processed after %d polls", id, 150)
}

package mastodon

import (
	"bytes"
	"context"

	gomast "github.com/mattn/go-mastodon"
)

// Image is one attachment. Mastodon detects the content type from the
// bytes server-side, so no MIME field is needed here.
type Image struct {
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
	InReplyToID string // when set, posts as a reply (threading)
}

type Result struct {
	RemoteID  string
	RemoteURL string
}

type Client struct{ c *gomast.Client }

func New(baseURL, token string) *Client {
	return &Client{c: gomast.NewClient(&gomast.Config{Server: baseURL, AccessToken: token})}
}

func (cl *Client) Post(ctx context.Context, p Post) (Result, error) {
	var mediaIDs []gomast.ID
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

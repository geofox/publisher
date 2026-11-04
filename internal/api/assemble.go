package api

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/dispatch"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/store"
	"github.com/geofox/publisher/internal/verify"
)

// sourceMediaClient is SSRF-guarded: source media URLs come from a pasted
// (untrusted) post, so a fetch must never reach internal/loopback addresses.
var sourceMediaClient = verify.NewSafeClient(20 * time.Second)

// assembleImages reconstructs the ordered image set for a publish/interact
// request from its spec.images, in spec order. Each entry is handled by kind:
//   - fresh upload (no BlossomURL): consume the next multipart "image" file and
//     run its bytes through the media pipeline;
//   - already-uploaded reference (BlossomURL set, no fresh file): re-fetch the
//     bytes from Blossom and re-process them, so platforms that need raw bytes
//     (Bluesky/Mastodon/Threads) still get the media and Nostr gets a fresh
//     imeta. Without this, a restored/loaded draft would publish with no media.
//
// A reference that can't be fetched is skipped (best-effort, matching source-
// media re-hosting) rather than failing the whole post. The returned slices are
// renumbered densely so a skipped reference doesn't leave an ordinal gap.
func (a *API) assembleImages(r *http.Request, specs []imageSpec) ([]dispatch.Img, []store.Media, error) {
	var files []*multipart.FileHeader
	if r.MultipartForm != nil {
		files = r.MultipartForm.File["image"]
	}
	// Every multipart file contributes at most one image; every Blossom-ref
	// spec entry contributes at most one more without consuming a file (the
	// trailing defensive loop processes leftover files). Bound the combined
	// total — but NOT len(specs)+len(files): a fresh-upload spec entry and the
	// file it consumes are the same image.
	blossomRefs := 0
	for _, s := range specs {
		if s.BlossomURL != "" {
			blossomRefs++
		}
	}
	if len(files)+blossomRefs > maxImagesPerRequest || len(specs) > maxImagesPerRequest {
		return nil, nil, fmt.Errorf("max %d images", maxImagesPerRequest)
	}
	fetch := a.fetchMedia
	if fetch == nil {
		fetch = a.fetchSourceMedia
	}

	// processUpload reads one multipart file and runs it through the media
	// pipeline. A pipeline failure is wrapped as *mediaError so callers can map
	// it to 502 (upstream) rather than 400 (client) — open/read failures stay
	// plain (client/server-side body issues).
	processUpload := func(fh *multipart.FileHeader) (media.Result, error) {
		f, err := fh.Open()
		if err != nil {
			return media.Result{}, fmt.Errorf("open image: %w", err)
		}
		body, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return media.Result{}, fmt.Errorf("read image: %w", err)
		}
		mime := fh.Header.Get("Content-Type")
		sniff := body
		if len(sniff) > 512 {
			sniff = sniff[:512]
		}
		if strings.HasPrefix(mime, "video/") || strings.HasPrefix(http.DetectContentType(sniff), "video/") {
			return media.Result{}, fmt.Errorf("video files must go through POST /api/media/video (async transcode pipeline)")
		}
		res, err := a.media.Process(r.Context(), body, mime)
		if err != nil {
			return media.Result{}, &mediaError{err: fmt.Errorf("media: %w", err)}
		}
		return res, nil
	}

	var imgs []dispatch.Img
	var recs []store.Media
	add := func(res media.Result, alt string) {
		imgs = append(imgs, dispatch.Img{Bytes: res.Bytes, Mime: res.Mime, Alt: alt, BlossomURL: res.URL,
			Dim: res.Dim, DurationSecs: res.DurationSecs, SizeBytes: res.Size})
		recs = append(recs, store.Media{
			Ordinal: len(recs), BlossomURL: res.URL, SHA256: res.SHA256, Mime: res.Mime,
			Dim: res.Dim, Blurhash: res.Blurhash, SizeBytes: res.Size, Alt: alt,
			DurationSecs: res.DurationSecs, PosterURL: res.PosterURL,
		})
	}

	fi := 0 // cursor into the uploaded files (fresh images, in spec order)
	for _, s := range specs {
		if s.BlossomURL != "" {
			if strings.HasPrefix(s.Mime, "video/") {
				// Video references are NEVER re-Processed (no canonical
				// mutation) and bytes are fetched only when a byte-upload
				// platform could actually use them (gates cap at ≤100 MB;
				// FetchCap is 110 MB). Larger canonicals ride as metadata-only:
				// URL platforms (threads, nostr) need nothing else and byte
				// platforms gate out first. A failed best-effort fetch leaves
				// Bytes nil — the adapter gate then fails that target loudly.
				img := dispatch.Img{Mime: s.Mime, Alt: s.Alt, BlossomURL: s.BlossomURL,
					Dim: s.Dim, DurationSecs: s.DurationSecs, SizeBytes: s.SizeBytes}
				if s.SizeBytes > 0 && s.SizeBytes <= media.FetchCap {
					if body, _, ferr := fetch(r.Context(), s.BlossomURL); ferr == nil {
						img.Bytes = body
					}
				}
				imgs = append(imgs, img)
				recs = append(recs, store.Media{
					Ordinal: len(recs), BlossomURL: s.BlossomURL, SHA256: s.SHA256, Mime: s.Mime,
					Dim: s.Dim, Blurhash: s.Blurhash, SizeBytes: s.SizeBytes, Alt: s.Alt,
					DurationSecs: s.DurationSecs, PosterURL: s.PosterURL,
				})
				continue
			}
			// Already-uploaded reference: re-fetch the bytes from Blossom.
			body, mime, ferr := fetch(r.Context(), s.BlossomURL)
			if ferr != nil {
				continue // unreachable reference → skip, never fail the post
			}
			res, perr := a.media.Process(r.Context(), body, mime)
			if perr != nil {
				continue
			}
			add(res, s.Alt)
			continue
		}
		// Fresh upload: consume the next multipart file part.
		if fi >= len(files) {
			continue // spec entry with neither file nor reference → skip
		}
		res, err := processUpload(files[fi])
		fi++
		if err != nil {
			return nil, nil, err
		}
		add(res, s.Alt)
	}
	// Defensive: process any uploaded files with no matching spec entry (a client
	// that posts files without an images[] array) as fresh images with no alt, so
	// media is never silently dropped.
	for ; fi < len(files); fi++ {
		res, err := processUpload(files[fi])
		if err != nil {
			return nil, nil, err
		}
		add(res, "")
	}
	return imgs, recs, nil
}

// mediaError marks a media-pipeline (upstream Blossom) failure so handlers can
// respond 502 Bad Gateway, distinct from 400 client errors.
type mediaError struct{ err error }

func (e *mediaError) Error() string { return e.err.Error() }

// fetchSourceMedia downloads a source media URL for re-hosting (https-only,
// size-limited, SSRF-guarded). Best-effort: callers skip on error.
func (a *API) fetchSourceMedia(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return nil, "", fmt.Errorf("bad media url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := sourceMediaClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("media fetch %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadRequestBytes))
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}

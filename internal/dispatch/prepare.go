package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/media"
	"github.com/geofox/publisher/internal/transcode"
)

// MediaHost uploads derived media bytes (a re-encoded variant) and returns a
// public URL. Destinations with URL transport (Threads) are given the variant
// URL. Wired to the media pipeline in main; nil keeps canonical URLs.
type MediaHost func(ctx context.Context, body []byte, mime string) (string, error)

// imageProfile builds the transcode profile an account's image limits imply.
// ok=false when the limits impose nothing (Nostr): images pass through.
func imageProfile(name string, l destination.Limit) (transcode.Profile, bool) {
	if l.MaxBytes == 0 && l.MaxPixels == 0 && len(l.Mimes) == 0 {
		return transcode.Profile{}, false
	}
	return transcode.Profile{Name: name, MaxBytes: l.MaxBytes, MaxPixels: l.MaxPixels, Mimes: l.Mimes, Quality: 85}, true
}

// VideoCheck reports a hard failure reason ("" = none) and advisories for a
// video against an account's video limits. Shared by dispatch (which fails
// the target before any network call) and the preview (which badges it).
//
// A byte upload also needs the video in memory, which media.FetchCap bounds
// whatever the destination announces (a Mastodon instance may allow more).
func VideoCheck(name string, mc destination.MediaCaps, sizeBytes, durationSecs int64) (fail string, warns []string) {
	l := mc.Video
	var fails []string
	if !l.Supported {
		fails = append(fails, name+" does not take video")
	}
	if mc.Transport == destination.TransportBytes && sizeBytes > media.FetchCap &&
		(l.MaxBytes == 0 || l.MaxBytes > media.FetchCap) {
		fails = append(fails, fmt.Sprintf("video over %s (the most Publisher can upload to %s)", humanBytes(media.FetchCap), name))
	}
	if l.MaxBytes > 0 && sizeBytes > l.MaxBytes {
		fails = append(fails, fmt.Sprintf("video over %s (%s limit)", humanBytes(l.MaxBytes), name))
	}
	if l.MaxDurationSecs > 0 && durationSecs > l.MaxDurationSecs {
		fails = append(fails, fmt.Sprintf("video over %s (%s limit, %ds)", humanSecs(l.MaxDurationSecs), name, durationSecs))
	}
	if l.AdvisoryDurationSecs > 0 && durationSecs > l.AdvisoryDurationSecs {
		warns = append(warns, fmt.Sprintf("over %s — %s may reject or truncate playback", humanSecs(l.AdvisoryDurationSecs), name))
	}
	return strings.Join(fails, "; "), warns
}

// gateVideo checks every attached video against the account's limits and its
// transport (byte uploads need the bytes; URL ingestion needs a URL). A limit
// breach is ClassInvalid. Missing bytes are transient: the size check has
// already passed, so the fetch from Blossom failed and may work next time.
func gateVideo(name string, mc destination.MediaCaps, imgs []Img) error {
	for i, im := range imgs {
		if !im.IsVideo() {
			continue
		}
		size := im.SizeBytes
		if size == 0 {
			size = int64(len(im.Bytes))
		}
		if fail, _ := VideoCheck(name, mc, size, im.DurationSecs); fail != "" {
			return destination.Invalidf("video %d: %s", i, fail)
		}
		if mc.Transport == destination.TransportBytes && len(im.Bytes) == 0 {
			return errBytesUnavailable("video", i, name)
		}
		if mc.Transport == destination.TransportURL && im.BlossomURL == "" {
			return destination.Invalidf("video %d: no canonical URL for %s (ingests by URL)", i, name)
		}
	}
	return nil
}

// errBytesUnavailable is a media fetch that failed: transient.
func errBytesUnavailable(kind string, i int, name string) error {
	return &destination.Error{Class: destination.ClassTransient,
		Err: fmt.Errorf("%s %d: bytes unavailable for %s (fetch from media storage failed)", kind, i, name)}
}

// splitVideo returns the first video attachment (nil if none) and the image
// attachments. The composer allows a video or images, and one video at most;
// extra videos are dropped.
func splitVideo(imgs []Img) (v *Img, images []Img) {
	for i := range imgs {
		if imgs[i].IsVideo() {
			if v == nil {
				v = &imgs[i]
			}
			continue
		}
		images = append(images, imgs[i])
	}
	return v, images
}

// prepareMedia turns a target's attachments into what the account takes:
// videos gated, images re-encoded when they break the account's image limits,
// and, for URL transport, a re-encoded variant hosted so the network can fetch
// it. The variant is content-addressed, so a retry converges on the same URL.
// Media that can't be made to fit is ClassInvalid (it would fail the same way
// again); missing bytes and a hosting failure are transient.
func prepareMedia(ctx context.Context, name string, mc destination.MediaCaps, imgs []Img, host MediaHost) ([]destination.MediaItem, error) {
	if err := gateVideo(name, mc, imgs); err != nil {
		return nil, err
	}
	v, images := splitVideo(imgs)
	var out []destination.MediaItem
	if v != nil {
		out = append(out, mediaItem(*v))
	}
	prof, fit := imageProfile(name, mc.Images)
	for i, im := range images {
		item := mediaItem(im)
		if len(im.Bytes) == 0 {
			if mc.Transport == destination.TransportBytes {
				return nil, errBytesUnavailable("image", i, name)
			}
			// URL transport without bytes: nothing to fit, the network
			// fetches the canonical object.
			out = append(out, item)
			continue
		}
		if fit {
			r, err := prof.Fit(im.Bytes, im.Mime)
			if err != nil {
				return nil, destination.Invalidf("fit image %d for %s: %w", i, name, err)
			}
			// A re-encode only helps a URL-transport network if the variant
			// is hosted; without a host it fetches the canonical object.
			if r.Changed && (mc.Transport != destination.TransportURL || host != nil) {
				sum := sha256.Sum256(r.Bytes)
				item.Bytes, item.Mime, item.SizeBytes = r.Bytes, r.Mime, int64(len(r.Bytes))
				item.SHA256 = hex.EncodeToString(sum[:])
				if r.W > 0 && r.H > 0 {
					item.Width, item.Height = r.W, r.H
				}
				if mc.Transport == destination.TransportURL {
					u, err := host(ctx, r.Bytes, r.Mime)
					if err != nil {
						// Hosting is an upstream failure, not bad input: retry.
						return nil, &destination.Error{Class: destination.ClassTransient,
							Err: fmt.Errorf("host %s variant %d: %w", name, i, err)}
					}
					item.URL = u
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

func mediaItem(im Img) destination.MediaItem {
	w, h := transcode.ParseDim(im.Dim)
	size := im.SizeBytes
	if size == 0 {
		size = int64(len(im.Bytes))
	}
	return destination.MediaItem{
		Mime: im.Mime, Alt: im.Alt, Bytes: im.Bytes, URL: im.BlossomURL, SHA256: im.SHA256,
		Width: w, Height: h, SizeBytes: size, DurationSecs: im.DurationSecs,
		Blurhash: im.Blurhash, PosterURL: im.PosterURL,
	}
}

// humanBytes renders a limit the way people write it: 100 MB, 99 MB, 1 GB.
func humanBytes(n int64) string {
	switch {
	case n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	case n%1_000_000 == 0:
		return fmt.Sprintf("%d MB", n/1_000_000)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	}
}

func humanSecs(s int64) string {
	if s%60 == 0 {
		return fmt.Sprintf("%d min", s/60)
	}
	return fmt.Sprintf("%ds", s)
}

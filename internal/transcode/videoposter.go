package transcode

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// PosterAt picks the poster frame time: 10% in, capped at 1 s — past any
// fade-from-black opener, deterministic for a given duration.
func PosterAt(durationSecs float64) float64 {
	t := durationSecs / 10
	if t > 1 {
		t = 1
	}
	return t
}

// posterArgs builds the single-frame extraction argv (pure, testable).
// -ss before -i = fast input seek; -q:v 4 ≈ visually-good JPEG; autorotation
// stays on so portrait posters match the (display-oriented) video frames.
func posterArgs(in, out string, atSecs float64) []string {
	return []string{
		"-hide_banner", "-nostats", "-nostdin",
		"-ss", strconv.FormatFloat(atSecs, 'f', -1, 64),
		"-i", in,
		"-frames:v", "1", "-q:v", "4",
		"-y", out,
	}
}

// ExtractPoster writes a single JPEG frame from the (already normalized)
// video. Callers treat failure as best-effort: a video without a poster is
// strictly better than a failed upload.
func ExtractPoster(ctx context.Context, ffmpegPath, in, out string, atSecs float64) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, posterArgs(in, out, atSecs)...)
	if b, err := cmd.CombinedOutput(); err != nil {
		tail := string(b)
		if len(tail) > 300 {
			tail = tail[len(tail)-300:]
		}
		return fmt.Errorf("poster: %w: %s", err, tail)
	}
	return nil
}

// ExtractPoster satisfies the videojob Transcoder seam.
func (e ExecTranscoder) ExtractPoster(ctx context.Context, in, out string, atSecs float64) error {
	return ExtractPoster(ctx, e.FFmpeg, in, out, atSecs)
}

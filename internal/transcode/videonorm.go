package transcode

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// NormParams: target dims from FitVideoDims (display-oriented, even, ≥2),
// fps = min(source, 60), audio presence from the probe, source duration for
// progress fractions.
type NormParams struct {
	W, H         int
	FPS          float64
	HasAudio     bool
	DurationSecs float64
}

// normalizeArgs builds the canonical-encode argv (pure, testable). Contract
// per spec §3.2: H.264 medium CRF23 maxrate 8M, scaled even dims, fps cap,
// yuv420p, AAC 128k 48kHz stereo (or -an), ALL metadata stripped (GPS!),
// faststart, machine-readable progress on stdout. Autorotation is REQUIRED
// (never -noautorotate): Probe reports display dims and ffmpeg 7.x feeds the
// filtergraph post-rotation frames — they must agree.
func normalizeArgs(in, out string, p NormParams) []string {
	fps := p.FPS
	if fps <= 0 || fps > 60 {
		fps = 60
	}
	args := []string{
		"-hide_banner", "-nostdin", "-y", "-i", in,
		"-map", "0:v:0",
		"-c:v", "libx264", "-preset", "medium", "-crf", "23",
		"-maxrate", "8M", "-bufsize", "16M",
		"-vf", fmt.Sprintf("scale=%d:%d", p.W, p.H),
		"-r", strconv.FormatFloat(fps, 'f', -1, 64),
		"-pix_fmt", "yuv420p",
	}
	if p.HasAudio {
		args = append(args, "-map", "0:a:0", "-c:a", "aac", "-b:a", "128k", "-ar", "48000", "-ac", "2")
	} else {
		args = append(args, "-an")
	}
	args = append(args, "-map_metadata", "-1", "-movflags", "+faststart",
		"-progress", "pipe:1", out)
	return args
}

// Normalize runs the one canonical transcode. progress (nil ok) receives
// fractions in (0,1] parsed from ffmpeg's -progress stream.
func Normalize(ctx context.Context, ffmpegPath, in, out string, p NormParams, progress func(float64)) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, normalizeArgs(in, out, p)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg start: %w", err)
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "out_time_us="); ok && progress != nil && p.DurationSecs > 0 {
			if us, err := strconv.ParseFloat(v, 64); err == nil {
				f := (us / 1e6) / p.DurationSecs
				if f > 1 {
					f = 1
				}
				if f > 0 {
					progress(f)
				}
			}
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg: %w", ctx.Err())
		}
		// stderr tail only — full ffmpeg logs are huge and may echo paths.
		tail := stderr.String()
		if len(tail) > 600 {
			tail = tail[len(tail)-600:]
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, tail)
	}
	if progress != nil {
		progress(1)
	}
	return nil
}

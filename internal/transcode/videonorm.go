package transcode

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// NormParams: target dims from FitVideoDims (display-oriented, even, ≥2),
// FPS: source rate; the builder clamps into Threads' 23–60 window (cap at 60,
// double-below-24), audio presence from the probe, source duration for
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
	// Threads requires 23–60 fps. Double sub-24 fps to stay in range while
	// preserving cadence via uniform frame duplication (15→30, 12→24).
	// Doubling a value <24 always yields <48, so we can never overshoot 60.
	for fps < 24 {
		fps *= 2
	}
	args := []string{
		"-hide_banner", "-nostats", "-nostdin", "-y", "-i", in,
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
	args = append(args, "-map_metadata", "-1", "-map_chapters", "-1", "-movflags", "+faststart",
		"-progress", "pipe:1", out)
	return args
}

// tailWriter keeps only the last N bytes written (ffmpeg stderr can grow for
// long encodes; only the tail ever reaches an error message).
type tailWriter struct {
	buf []byte
	max int
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

// Normalize runs the one canonical transcode. progress (nil ok) receives
// fractions in (0,1] parsed from ffmpeg's -progress stream.
func Normalize(ctx context.Context, ffmpegPath, in, out string, p NormParams, progress func(float64)) error {
	cmd := exec.CommandContext(ctx, ffmpegPath, normalizeArgs(in, out, p)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	st := &tailWriter{max: 600}
	cmd.Stderr = st
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
	// Normally instant EOF; if the scanner aborted early (read error), this
	// keeps draining so ffmpeg can't block on a full progress pipe — the
	// encode then finishes and Wait reports the real outcome.
	io.Copy(io.Discard, stdout) //nolint:errcheck
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg: %w", ctx.Err())
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(string(st.buf)))
	}
	if progress != nil {
		progress(1)
	}
	return nil
}

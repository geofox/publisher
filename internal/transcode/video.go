package transcode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
)

// ExecTranscoder satisfies videojob.Transcoder with the real binaries.
type ExecTranscoder struct{ FFmpeg, FFprobe string }

func NewExecTranscoder(ffmpeg, ffprobe string) ExecTranscoder {
	return ExecTranscoder{FFmpeg: ffmpeg, FFprobe: ffprobe}
}
func (e ExecTranscoder) Probe(ctx context.Context, path string) (VideoMeta, error) {
	return Probe(ctx, e.FFprobe, path)
}
func (e ExecTranscoder) Normalize(ctx context.Context, in, out string, p NormParams, progress func(float64)) error {
	return Normalize(ctx, e.FFmpeg, in, out, p, progress)
}

// VideoMeta is the probed shape of a video file. W, H are display
// (rotation-corrected) dimensions. DurationSecs is fractional; store rows
// round up (a 2.5 s clip gates as 3 s — conservative). FPS is the average
// frame rate; container nominal rate as fallback; 0 when unknowable.
type VideoMeta struct {
	W, H         int
	DurationSecs float64
	FPS          float64
	VCodec       string
	ACodec       string
	HasAudio     bool
}

// Probe runs ffprobe and parses its JSON. The binary path comes from config
// (container: /usr/local/bin/ffprobe; dev/tests: $PATH lookup).
func Probe(ctx context.Context, ffprobePath, file string) (VideoMeta, error) {
	cmd := exec.CommandContext(ctx, ffprobePath,
		"-v", "error", "-print_format", "json", "-show_streams", "-show_format", file)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return VideoMeta{}, fmt.Errorf("ffprobe: %w", ctx.Err())
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg := string(bytes.TrimSpace(ee.Stderr))
			if len(msg) > 300 {
				msg = msg[:300]
			}
			return VideoMeta{}, fmt.Errorf("ffprobe: %w: %s", err, msg)
		}
		return VideoMeta{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseProbe(out)
}

// parseProbe is the pure half of Probe, separated for binary-free testing.
func parseProbe(b []byte) (VideoMeta, error) {
	var raw struct {
		Streams []struct {
			CodecType   string `json:"codec_type"`
			CodecName   string `json:"codec_name"`
			Width       int    `json:"width"`
			Height      int    `json:"height"`
			RFrame      string `json:"r_frame_rate"`
			AvgFrame    string `json:"avg_frame_rate"`
			Disposition struct {
				AttachedPic int `json:"attached_pic"`
			} `json:"disposition"`
			SideData []struct {
				Rotation float64 `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return VideoMeta{}, fmt.Errorf("parse ffprobe: %w", err)
	}
	var m VideoMeta
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			if s.Disposition.AttachedPic == 1 {
				continue // cover art embedded in audio files is not a video stream
			}
			if m.VCodec == "" { // first real video stream wins
				m.VCodec, m.W, m.H = s.CodecName, s.Width, s.Height
				m.FPS = parseRate(s.AvgFrame)
				if m.FPS <= 0 {
					m.FPS = parseRate(s.RFrame)
				}
				// Phone video is usually landscape-coded + a Display Matrix rotation;
				// ffmpeg 7.x autorotates on transcode (the filtergraph sees post-rotation
				// frames), so W/H here are DISPLAY dimensions — rotation-corrected — and
				// every downstream consumer (fit boxes, scale args, stored dim, gates)
				// stays rotation-oblivious. Constraint for the normalize task: never pass
				// -noautorotate, or these dims and the frames diverge.
				for _, sd := range s.SideData {
					if sd.Rotation != 0 {
						rot := int(math.Round(sd.Rotation))
						if ((rot%180)+180)%180 == 90 {
							m.W, m.H = m.H, m.W
						}
						break
					}
				}
			}
		case "audio":
			if !m.HasAudio {
				m.HasAudio, m.ACodec = true, s.CodecName
			}
		}
	}
	if m.VCodec == "" || m.W <= 0 || m.H <= 0 {
		return VideoMeta{}, fmt.Errorf("no decodable video stream")
	}
	m.DurationSecs, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	if m.DurationSecs <= 0 {
		return VideoMeta{}, fmt.Errorf("missing/zero duration")
	}
	return m, nil
}

// parseRate turns ffprobe's "30000/1001" fraction into a float; 0 on garbage.
func parseRate(s string) float64 {
	num, den, ok := strings.Cut(s, "/")
	if !ok {
		f, _ := strconv.ParseFloat(s, 64)
		return f
	}
	n, _ := strconv.ParseFloat(num, 64)
	d, _ := strconv.ParseFloat(den, 64)
	if d == 0 {
		return 0
	}
	return n / d
}

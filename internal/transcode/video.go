package transcode

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// VideoMeta is the probed shape of a video file. DurationSecs is fractional;
// store rows round up (a 2.5 s clip gates as 3 s — conservative).
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
		return VideoMeta{}, fmt.Errorf("ffprobe: %w", err)
	}
	return parseProbe(out)
}

// parseProbe is the pure half of Probe, separated for binary-free testing.
func parseProbe(b []byte) (VideoMeta, error) {
	var raw struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			RFrame    string `json:"r_frame_rate"`
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
			if m.VCodec == "" { // first video stream wins
				m.VCodec, m.W, m.H = s.CodecName, s.Width, s.Height
				m.FPS = parseRate(s.RFrame)
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

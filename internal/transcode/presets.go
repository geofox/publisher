package transcode

// PresetParams maps a composer preset name to transcode parameters; ok=false
// for unknown names. "convert" is the HEIC auto-conversion (full size, high
// quality); the pixel presets bound the long edge to tame phone photos. All
// presets force JPEG so output format is predictable and metadata is stripped.
// "original" is intentionally absent — reverting is a client-side swap back to
// the kept original File, never a server round-trip.
func PresetParams(name string) (ImageParams, bool) {
	switch name {
	case "convert":
		return ImageParams{Format: JPEG, Quality: 90}, true
	case "large":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 2048}, true
	case "medium":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 1600}, true
	case "small":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 1080}, true
	}
	return ImageParams{}, false
}

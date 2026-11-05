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
		// MaxBytes guards against JPEG inflation (HEIC is ~2x denser): output
		// must stay under the 64 MB pipeline Fetch/upload cap or re-fetched
		// drafts would silently truncate. The ladder only kicks in for
		// pathological inputs; ordinary phone photos never hit it.
		return ImageParams{Format: JPEG, Quality: 90, MaxBytes: 64 << 20}, true
	case "large":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 2048}, true
	case "medium":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 1600}, true
	case "small":
		return ImageParams{Format: JPEG, Quality: 82, MaxLongEdge: 1080}, true
	}
	return ImageParams{}, false
}

// ImagePresetNames lists the names PresetParams accepts.
var ImagePresetNames = []string{"convert", "large", "medium", "small"}

// VideoPresetNames lists the video presets, largest first.
var VideoPresetNames = []string{"1080p", "720p", "480p"}

// IsVideoPreset reports whether name is a video preset FitVideoDims knows.
func IsVideoPreset(name string) bool {
	_, ok := videoBoxes[name]
	return ok
}

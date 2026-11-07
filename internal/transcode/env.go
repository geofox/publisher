package transcode

import "strings"

// subprocessEnvKeep are the variables ffmpeg and ffprobe may need; every
// other one (credentials among them) stays out of their environment.
var subprocessEnvKeep = []string{"PATH", "TMPDIR", "TZ", "LANG", "LC_ALL", "HOME"}

// subprocessEnv is environ (os.Environ) reduced to subprocessEnvKeep.
func subprocessEnv(environ []string) []string {
	out := []string{}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		for _, keep := range subprocessEnvKeep {
			if k == keep {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

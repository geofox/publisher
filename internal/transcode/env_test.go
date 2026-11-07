package transcode

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSubprocessEnvKeepsNoSecret(t *testing.T) {
	got := subprocessEnv([]string{"PATH=/bin", "NSEC_HEX=abc", "PUBLISHER_MASTER_KEY=k", "TZ=UTC", "MASTODON_TOKEN=t", "PATHX=1"})
	if !slices.Equal(got, []string{"PATH=/bin", "TZ=UTC"}) {
		t.Errorf("env %v", got)
	}
}

// The real commands run with that environment: a fake ffprobe that
// prints its environment sees no secret.
func TestProbeRunsWithoutSecrets(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffprobe")
	script := "#!" + sh + "\nenv > " + filepath.Join(dir, "env.txt") + "\necho '{}'\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NSEC_HEX", "deadbeef")
	t.Setenv("PUBLISHER_MASTER_KEY", "secretkey")
	_, _ = Probe(context.Background(), fake, filepath.Join(dir, "in.mp4"))
	b, err := os.ReadFile(filepath.Join(dir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); strings.Contains(s, "deadbeef") || strings.Contains(s, "secretkey") {
		t.Errorf("subprocess saw a secret:\n%s", s)
	}
}

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startRun runs the server with env (plus a temporary database, any free
// port and no ffmpeg) and returns its base URL and a stop function that
// waits for a graceful stop and returns the exit code.
func startRun(t *testing.T, env map[string]string) (string, func() int) {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{
		"DB_PATH":      filepath.Join(dir, "p.db"),
		"PORT":         "0",
		"FFMPEG_PATH":  filepath.Join(dir, "no-ffmpeg"),
		"FFPROBE_PATH": filepath.Join(dir, "no-ffprobe"),
		"LOG_LEVEL":    "error",
	}
	for k, v := range env {
		base[k] = v
	}
	ctx, cancel := context.WithCancel(context.Background())
	addr := make(chan string, 1)
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, runOptions{Getenv: func(k string) string { return base[k] }, Started: func(a string) { addr <- a }})
	}()
	var url string
	select {
	case a := <-addr:
		url = "http://127.0.0.1:" + a[strings.LastIndex(a, ":")+1:]
	case code := <-done:
		cancel()
		t.Fatalf("run exited with %d before serving", code)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("run didn't start serving")
	}
	stopped := false
	stop := func() int {
		if stopped {
			return 0
		}
		stopped = true
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(40 * time.Second):
			t.Fatal("run didn't stop")
			return -1
		}
	}
	t.Cleanup(func() { stop() })
	return url, stop
}

func get(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if v != nil {
		if err := json.Unmarshal(b, v); err != nil {
			t.Fatalf("%s: %v (%s)", url, err, b)
		}
	}
	return resp.StatusCode
}

// A server with only a Bluesky account (no Nostr key, no media store)
// starts, is ready, says it has no media, and stops gracefully.
func TestRunWithOnlyBluesky(t *testing.T) {
	url, stop := startRun(t, map[string]string{
		"BLUESKY_APP_PASSWORD": "aaaa-bbbb-cccc-dddd",
		"BLUESKY_IDENTIFIER":   "me.example",
		"BLUESKY_PDS_URL":      "http://127.0.0.1:1",
	})
	if code := get(t, url+"/readyz", nil); code != 200 {
		t.Fatalf("/readyz = %d", code)
	}
	var inst struct {
		Features struct {
			Media bool `json:"media"`
			Video bool `json:"video"`
		} `json:"features"`
	}
	if code := get(t, url+"/v1/instance", &inst); code != 200 || inst.Features.Media || inst.Features.Video {
		t.Fatalf("/v1/instance = %d %+v", code, inst)
	}
	var accts struct {
		Data []struct{ Type, Status string }
	}
	if code := get(t, url+"/v1/accounts", &accts); code != 200 || len(accts.Data) != 1 || accts.Data[0].Status != "connected" {
		t.Fatalf("/v1/accounts = %d %+v", code, accts)
	}
	resp, err := http.Post(url+"/v1/media", "image/png", strings.NewReader("\x89PNG\r\n\x1a\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(b), "no media store is configured") {
		t.Fatalf("upload = %d %s", resp.StatusCode, b)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit code %d after a graceful stop", code)
	}
}

// A server with nothing configured starts too (accounts come later).
func TestRunWithNothingConfigured(t *testing.T) {
	url, _ := startRun(t, nil)
	if code := get(t, url+"/readyz", nil); code != 200 {
		t.Fatalf("/readyz = %d", code)
	}
}

// A media store needs the key that signs uploads to it.
func TestRunBlossomWithoutNostrKey(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	env := map[string]string{"BLOSSOM_URL": "https://blossom.example", "DB_PATH": filepath.Join(t.TempDir(), "p.db")}
	if code := run(ctx, runOptions{Getenv: func(k string) string { return env[k] }}); code != 2 {
		t.Fatalf("exit code %d, want 2 (configuration error)", code)
	}
}

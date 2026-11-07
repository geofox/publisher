package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/geofox/publisher/internal/secrets"
	"github.com/geofox/publisher/internal/store"
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
		"AUTH_MODE":            "proxy", // no sign-in: the test calls the API directly
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
	if resp.StatusCode != 503 || !strings.Contains(string(b), "no media store is set") {
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

// A Blossom server set without a key to sign uploads with: the server
// starts, says it has media, and an upload says to make a key.
func TestRunBlossomWithoutNostrKey(t *testing.T) {
	url, stop := startRun(t, map[string]string{"AUTH_MODE": "proxy", "BLOSSOM_URL": "https://blossom.example"})
	var inst struct {
		Features struct {
			Media bool `json:"media"`
		} `json:"features"`
	}
	if code := get(t, url+"/v1/instance", &inst); code != 200 || !inst.Features.Media {
		t.Fatalf("/v1/instance = %d %+v", code, inst)
	}
	resp, err := http.Post(url+"/v1/media", "image/png", strings.NewReader(string(tinyPNG(t))))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 503 || !strings.Contains(string(b), "make a signing key in Settings, Media") {
		t.Fatalf("upload = %d %s", resp.StatusCode, b)
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit code %d after a graceful stop", code)
	}
}

// The master key's status is reported at /v1/instance and in /readyz (not
// critical: accounts from the environment work): an account saved under
// one key is locked under another key or none, and opens under its own.
func TestRunMasterKeyStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "p.db")
	keyA, keyB := secrets.Generate(), secrets.Generate()
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := secrets.ParseMasterKey(keyA)
	k, _ := secrets.NewKey(raw)
	id := store.NewAccountID()
	b := secrets.Binding{Owner: store.DefaultOwner, Scope: store.SecretScopeAccount, Ref: id, Name: "access_token", Audience: "https://m.example"}
	sealed, err := secrets.Keyring{Current: k}.Seal(b, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAccountWithSecrets(context.Background(), store.NewAccount{ID: id, OwnerID: store.DefaultOwner, Type: "mastodon",
		Label: "Work", AuthMethod: "token", Config: map[string]any{"instance_url": "https://m.example"}, Actor: "test",
		Secrets: []store.SealedSecret{{Owner: b.Owner, Scope: b.Scope, Ref: id, Name: b.Name, Audience: b.Audience, Sealed: sealed, KeyID: k.ID()}}}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	for _, c := range []struct {
		key, status, check, account string
	}{
		{keyB, "mismatch", "master key mismatch", "locked"},
		{"", "missing", "master key missing", "locked"},
		{keyA, "ok", "", "connected"},
	} {
		url, stop := startRun(t, map[string]string{"AUTH_MODE": "proxy", "DB_PATH": dbPath, "PUBLISHER_MASTER_KEY": c.key})
		var ready struct {
			Status string
			Checks map[string]struct{ Status, Error string }
		}
		if code := get(t, url+"/readyz", &ready); code != 200 || ready.Checks["secrets"].Error != c.check {
			t.Errorf("%s: /readyz = %d %+v", c.status, code, ready)
		}
		var inst struct {
			Secrets struct{ Status, Detail string }
		}
		if code := get(t, url+"/v1/instance", &inst); code != 200 || inst.Secrets.Status != c.status || (c.status != "ok") != (inst.Secrets.Detail != "") {
			t.Errorf("%s: /v1/instance = %d %+v", c.status, code, inst.Secrets)
		}
		for _, v := range []string{keyA, keyB, "tok"} {
			if strings.Contains(inst.Secrets.Detail, v) {
				t.Errorf("%s: the detail holds a secret", c.status)
			}
		}
		var accts struct {
			Data []struct{ ID, Status string }
		}
		if code := get(t, url+"/v1/accounts", &accts); code != 200 || len(accts.Data) != 1 || accts.Data[0].Status != c.account {
			t.Errorf("%s: /v1/accounts = %d %+v", c.status, code, accts)
		}
		if code := stop(); code != 0 {
			t.Fatalf("exit code %d", code)
		}
	}
}

// The server's log scrubs credentials from what it prints: a transport
// error carrying a token in its URL, an nsec1 key in the message.
func TestLoggerScrubs(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	setupLogger("info", &buf)
	err := &url.Error{Op: "Post", URL: "https://graph.example.com/x?access_token=THAATOKEN123", Err: errors.New("timeout")}
	slog.Error("refresh failed for nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5", "err", err)
	if out := buf.String(); strings.Contains(out, "THAATOKEN123") || strings.Contains(out, "vl029mgpspedva04") || !strings.Contains(out, "timeout") {
		t.Errorf("log: %s", out)
	}
}

// tinyPNG is a 2×2 PNG.
func tinyPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

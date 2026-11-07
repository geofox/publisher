package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"
)

func TestContextHandlerInjectsAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})

	ctx := With(context.Background(), "post_id", "abc123")
	logger.InfoContext(ctx, "fired")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["post_id"] != "abc123" {
		t.Fatalf("post_id = %v, want abc123", rec["post_id"])
	}
}

func TestNoContextValueIsHarmless(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(ContextHandler{Handler: slog.NewJSONHandler(&buf, nil)})
	logger.InfoContext(context.Background(), "plain") // must not panic
	if buf.Len() == 0 {
		t.Fatal("expected a log line")
	}
}

// Every string, error, Stringer and string list the log prints is
// scrubbed, the message and grouped and pre-set attributes included; other
// values print as before.
func TestScrubAttr(t *testing.T) {
	var buf bytes.Buffer
	h := ContextHandler{Handler: slog.NewJSONHandler(&buf, &slog.HandlerOptions{ReplaceAttr: ScrubAttr})}
	logger := slog.New(h).With("pre", "Bearer PRESET1")
	u, _ := url.Parse("https://graph.example.com/refresh?grant_type=x&access_token=URLTOK2")
	urlErr := &url.Error{Op: "Get", URL: u.String(), Err: errors.New("dial tcp: i/o timeout")}
	ctx := With(context.Background(), "ctx", "password=CTXPW3")
	logger.InfoContext(ctx, "failed with nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5",
		"err", urlErr, "url", u, "list", []string{"ok", `{"app_password":"LIST4"}`},
		slog.Group("g", "body", `{"access_token":"GROUP5"}`), "n", 7, "ok", "plain words")
	out := buf.String()
	for _, secret := range []string{"PRESET1", "URLTOK2", "CTXPW3", "vl029mgpspedva04", "LIST4", "GROUP5"} {
		if strings.Contains(out, secret) {
			t.Errorf("%s leaked: %s", secret, out)
		}
	}
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if rec["n"] != float64(7) || rec["ok"] != "plain words" || !strings.Contains(rec["err"].(string), "i/o timeout") ||
		!strings.Contains(rec["msg"].(string), "failed with") {
		t.Errorf("record: %v", rec)
	}
}

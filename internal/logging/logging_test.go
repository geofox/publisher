package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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

package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

// TestLogItemResult pins the per-attempt log line the README's Logging
// section documents, since the old /publish summary line went with it.
func TestLogItemResult(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	c := &store.Claim{Platform: "p", Ordinal: 1, Attempts: 2}
	logItemResult(context.Background(), c, store.ItemResult{Status: store.ItemPublished, RemoteID: "r1",
		Deliveries: []store.RelayState{{URL: "a", Status: "ok"}, {URL: "b", Status: "failed"}}})
	logItemResult(context.Background(), c, store.ItemResult{Status: store.ItemFailed, ErrorClass: "transient", Error: "boom"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	var ok, bad map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &ok)
	_ = json.Unmarshal([]byte(lines[1]), &bad)
	if ok["msg"] != "engine: item sent" || ok["level"] != "INFO" || ok["remote_id"] != "r1" ||
		ok["attempt"] != 3.0 || ok["endpoints_ok"] != 1.0 || ok["endpoints_failed"] != 1.0 {
		t.Errorf("sent line: %v", ok)
	}
	if bad["msg"] != "engine: item not sent" || bad["level"] != "WARN" || bad["error_class"] != "transient" ||
		bad["error"] != "boom" || bad["give_up"] != false {
		t.Errorf("failed line: %v", bad)
	}
}

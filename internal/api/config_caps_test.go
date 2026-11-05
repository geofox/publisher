package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// /api/config carries each destination's name and effective capabilities:
// a connected account's (a Mastodon instance's own limit) over the driver
// defaults.
func TestHandleConfigDestinations(t *testing.T) {
	a := &API{Caps: func(_ context.Context, name string) (destination.Capabilities, bool) {
		if name != "mastodon" {
			return destination.Capabilities{}, false
		}
		d, _ := destination.Lookup("mastodon")
		c := d.Manifest().Defaults
		c.Text.MaxLength = 5000
		return c, true
	}}
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	var got struct {
		Destinations map[string]struct {
			Name         string                   `json:"name"`
			Capabilities destination.Capabilities `json:"capabilities"`
		} `json:"destinations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m := got.Destinations["mastodon"]; m.Name != "Mastodon" || m.Capabilities.Text.MaxLength != 5000 {
		t.Errorf("mastodon = %+v, want the account's 5000", m)
	}
	if b := got.Destinations["bluesky"]; b.Capabilities.Text.MaxLength != 300 || b.Capabilities.LinkPreview != "client" {
		t.Errorf("bluesky = %+v, want the driver defaults", b)
	}
	if len(got.Destinations) != 4 {
		t.Errorf("want the 4 built-in destinations, got %d", len(got.Destinations))
	}
}

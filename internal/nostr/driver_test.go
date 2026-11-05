package nostr

import (
	"strings"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

func TestImetas(t *testing.T) {
	got := Imetas([]destination.MediaItem{
		{URL: "https://b/x", Mime: "image/png", SHA256: "aa", Width: 10, Height: 10, Blurhash: "LEH", Alt: "A blue\n  gradient "},
		{Mime: "image/png"}, // no URL: nothing to point at
		{URL: "https://b/v", Mime: "video/mp4", SHA256: "bb", PosterURL: "https://b/p"},
	})
	if len(got) != 2 {
		t.Fatalf("want one tag per item with a URL, got %d", len(got))
	}
	if s := strings.Join(got[0], " "); s != "imeta url https://b/x m image/png x aa dim 10x10 blurhash LEH alt A blue gradient" {
		t.Errorf("imeta[0] = %q", s)
	}
	for _, f := range got[1] {
		if strings.HasPrefix(f, "alt") {
			t.Errorf("an item without alt text has an alt field: %v", got[1])
		}
	}
	if s := strings.Join(got[1], " "); !strings.Contains(s, "image https://b/p") {
		t.Errorf("video imeta should carry its poster: %q", s)
	}
}

func TestStatusFromDeliveries(t *testing.T) {
	d := func(ss ...string) []destination.Delivery {
		var out []destination.Delivery
		for _, s := range ss {
			out = append(out, destination.Delivery{Status: s})
		}
		return out
	}
	cases := []struct {
		in   []destination.Delivery
		want destination.ItemStatus
	}{
		{d("ok", "ok"), destination.StatusPublished},
		{d("ok", "failed"), destination.StatusPartial},
		{d("failed", "failed"), destination.StatusFailed},
		{d("ok", "ok", "skipped"), destination.StatusPublished}, // skipped relays don't count
		{d("failed", "skipped"), destination.StatusFailed},
		{d("skipped"), destination.StatusFailed}, // nothing attempted
	}
	for _, c := range cases {
		if got := StatusFromDeliveries(c.in); got != c.want {
			t.Errorf("%v → %s, want %s", c.in, got, c.want)
		}
	}
}

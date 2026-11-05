// Package drivers registers Publisher's built-in destination drivers. It is
// the one place the core's binaries name concrete destination types; the
// dispatcher, API and web app work from manifests and capabilities.
package drivers

import (
	"sync"

	"github.com/geofox/publisher/internal/bluesky"
	"github.com/geofox/publisher/internal/destination"
	"github.com/geofox/publisher/internal/mastodon"
	"github.com/geofox/publisher/internal/nostr"
	"github.com/geofox/publisher/internal/threads"
)

var once sync.Once

// Register registers the built-in drivers. Safe to call more than once.
func Register() {
	once.Do(func() {
		for _, d := range []destination.Driver{bluesky.Driver{}, mastodon.Driver{}, nostr.Driver{}, threads.Driver{}} {
			destination.Register(d)
		}
	})
}

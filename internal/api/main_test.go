package api

import (
	"os"
	"testing"

	"github.com/geofox/publisher/internal/drivers"
)

// The preview and config handlers read destination limits from the
// registered drivers, as in production.
func TestMain(m *testing.M) {
	drivers.Register()
	os.Exit(m.Run())
}

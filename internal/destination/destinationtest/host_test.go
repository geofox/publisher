package destinationtest

import (
	"context"
	"reflect"
	"testing"

	"github.com/geofox/publisher/internal/destination"
)

// The test host merges saves as a real host does.
func TestHostMergesSaves(t *testing.T) {
	h := NewHost(t).WithState(map[string]string{"token": "a", "other": "b"}, map[string]any{"user_id": "me"})
	_ = h.Save(context.Background(), destination.StateUpdate{Secrets: map[string]string{"token": "c"}, Config: map[string]any{"expires_at": "x"}})
	s, c := h.State()
	if !reflect.DeepEqual(s, map[string]string{"token": "c", "other": "b"}) || !reflect.DeepEqual(c, map[string]any{"user_id": "me", "expires_at": "x"}) {
		t.Errorf("state = %v, %v", s, c)
	}
	if len(h.Saved()) != 1 {
		t.Errorf("saved = %v", h.Saved())
	}
}

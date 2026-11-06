package identity

import (
	"context"
	"testing"
)

type qaProf struct{}

func (qaProf) Profile(ctx context.Context) (string, string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", "", err
	}
	return "@me", "Me", "https://a/x.png", nil
}

// A request whose context is already canceled (the browser navigated away)
// gets nothing from every platform, and that empty identity is cached for
// the whole TTL: the next, healthy request is served the empty one.
func TestCanceledRequestPoisonsCache(t *testing.T) {
	s := &Service{Bluesky: qaProf{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Get(ctx)
	if got := s.Get(context.Background()); got.Name != "Me" {
		t.Fatalf("healthy request after a canceled one got %+v; want the profile", got)
	}
}

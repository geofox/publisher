package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Many concurrent writers on one file DB must never see SQLITE_BUSY.
func TestConcurrentWritersNoBusy(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var wg sync.WaitGroup
	var fails atomic.Int64
	var firstErr atomic.Value
	for g := 0; g < 24; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				d := &V1Draft{Title: "t", Text: "x", Body: `{"content":{"text":"x"}}`}
				if err := s.CreateV1Draft(ctx, d); err != nil {
					fails.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
					continue
				}
				d.Body = fmt.Sprintf(`{"content":{"text":"%d"}}`, i)
				if err := s.UpdateV1Draft(ctx, d, d.Version); err != nil {
					fails.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				}
				if _, err := s.ListV1Drafts(ctx, V1DraftQuery{OwnerID: DefaultOwner, Limit: 5}); err != nil {
					fails.Add(1)
					firstErr.CompareAndSwap(nil, err.Error())
				}
			}
		}(g)
	}
	wg.Wait()
	if n := fails.Load(); n > 0 {
		t.Fatalf("%d failures, first: %v", n, firstErr.Load())
	}
}

// UpdateV1Draft must hand back the version its own write produced. It
// re-reads the row after the UPDATE outside any transaction, so a write
// landing in between is returned as if it were ours: the caller's ETag then
// covers content it never saw, and its next If-Match save silently
// overwrites the other writer.
func TestUpdateV1DraftReturnsForeignVersion(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	base := &V1Draft{Title: "t", Text: "x", Body: `{"content":{"text":"x"}}`}
	if err := s.CreateV1Draft(ctx, base); err != nil {
		t.Fatal(err)
	}
	var wrong atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 400; i++ {
				cur, err := s.GetV1Draft(ctx, DefaultOwner, base.ID)
				if err != nil {
					t.Error(err)
					return
				}
				want := cur.Version
				mine := fmt.Sprintf(`{"content":{"text":"g%d-%d"}}`, g, i)
				cur.Body = mine
				if err := s.UpdateV1Draft(ctx, cur, want); err != nil {
					continue // a 412: fine
				}
				if cur.Version != want+1 || cur.Body != mine {
					wrong.Add(1)
				}
			}
		}(g)
	}
	wg.Wait()
	if n := wrong.Load(); n > 0 {
		t.Fatalf("%d successful saves returned another writer's version/body (ETag not for our write)", n)
	}
}

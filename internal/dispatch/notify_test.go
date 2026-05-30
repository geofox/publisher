package dispatch

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/geofox/publisher/internal/store"
)

type recordingNotifier struct{ posts []*store.Post }

func (r *recordingNotifier) PostPublished(_ context.Context, p *store.Post) {
	r.posts = append(r.posts, p)
}

func TestPostNotifiesOnImmediatePublish(t *testing.T) {
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Notify: rn}
	rec := d.Post(context.Background(), PostSpec{MasterText: "hi", Platforms: []string{"nostr"}, Source: "web"})
	if len(rn.posts) != 1 || rn.posts[0].ID != rec.ID {
		t.Fatalf("PostPublished calls = %d, want 1 with id %s", len(rn.posts), rec.ID)
	}
}

func TestInteractNotifies(t *testing.T) {
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Notify: rn}
	rec := d.Interact(context.Background(), InteractSpec{
		Action: "quote", SourcePlatform: "nostr", Text: "nice",
		Ref: InteractRef{EventID: "e1", Author: "a1"},
	})
	if len(rn.posts) != 1 || rn.posts[0].ID != rec.ID {
		t.Fatalf("Interact PostPublished calls = %d, want 1 with id %s", len(rn.posts), rec.ID)
	}
}

func TestRetryNotifies(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A post whose nostr target failed, so Retry will re-run it.
	if err := db.SavePost(&store.Post{
		ID: "p1", Platforms: []string{"nostr"}, Status: "failed",
		Targets: []store.Target{{Platform: "nostr", Status: "failed",
			Attempts: []store.Attempt{{AttemptNo: 1, Status: "failed"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Store: db, Notify: rn}
	if _, err := d.Retry(context.Background(), "p1", nil); err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if len(rn.posts) != 1 || rn.posts[0].ID != "p1" {
		t.Fatalf("Retry PostPublished calls = %d, want 1 with id p1", len(rn.posts))
	}
}

func TestFireNotifies(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SavePost(&store.Post{
		ID: "p2", Platforms: []string{"nostr"}, Status: "scheduled",
		Targets: []store.Target{{Platform: "nostr", Status: "scheduled"}},
	}); err != nil {
		t.Fatal(err)
	}
	rn := &recordingNotifier{}
	d := &Dispatcher{Nostr: fakeNostr{}, Store: db, Notify: rn}
	if _, err := d.Fire(context.Background(), "p2"); err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if len(rn.posts) != 1 || rn.posts[0].ID != "p2" {
		t.Fatalf("Fire PostPublished calls = %d, want 1 with id p2", len(rn.posts))
	}
}

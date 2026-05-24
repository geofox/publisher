package store

import (
	"path/filepath"
	"testing"
)

func TestListPostsSearchEscapesLikeWildcards(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mkPost(t, db, "a", "success", "discount 50% today")    // literal "50%"
	mkPost(t, db, "e", "success", "order 5012 shipped")    // has "50", not "50%"
	mkPost(t, db, "c", "success", "snake_case identifier") // literal "snake_case"
	mkPost(t, db, "d", "success", "snakeXcase identifier") // matches "snake_case" only if "_" is a wildcard

	// "%" is escaped → matched literally, so only post "a" qualifies (not "e").
	got, err := db.ListPostsFiltered(PostFilter{Query: "50%", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf(`query "50%%": got %d posts, want exactly [a]`, len(got))
	}

	// "_" is escaped → matched literally, so "snakeXcase" must NOT match.
	got, err = db.ListPostsFiltered(PostFilter{Query: "snake_case", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c" {
		t.Fatalf(`query "snake_case": got %d posts, want exactly [c]`, len(got))
	}
}

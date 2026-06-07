package store

import "testing"

func TestAuthTablesExist(t *testing.T) {
	s := openTestStore(t)
	for _, tbl := range []string{"users", "sessions", "api_tokens"} {
		var name string
		err := s.sql.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl,
		).Scan(&name)
		if err != nil {
			t.Fatalf("table %q missing: %v", tbl, err)
		}
	}
}

func TestUpsertUser(t *testing.T) {
	s := openTestStore(t)
	u1, err := s.UpsertUser("sub-1", "a@example.com", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	if u1.ID == "" || u1.Subject != "sub-1" {
		t.Fatalf("bad user: %+v", u1)
	}
	u2, err := s.UpsertUser("sub-1", "alice@example.com", "Alice B")
	if err != nil {
		t.Fatal(err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("id changed on re-upsert: %s != %s", u2.ID, u1.ID)
	}
	if u2.Email != "alice@example.com" || u2.Name != "Alice B" {
		t.Fatalf("not updated: %+v", u2)
	}
}

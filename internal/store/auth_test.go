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

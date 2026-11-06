package store

import (
	"context"
	"strings"
	"testing"
)

// saveWithAccounts saves p after giving each target without an account
// the test account of its type (acc_<type>), created when missing: tests
// that aren't about accounts name targets by type.
func saveWithAccounts(s *Store, p *Post) error {
	for i := range p.Targets {
		if p.Targets[i].AccountID != "" {
			continue
		}
		id, err := testAccount(s, p.Targets[i].Platform)
		if err != nil {
			return err
		}
		p.Targets[i].AccountID = id
	}
	return s.SavePost(p)
}

// testAccount returns the id of the test account of typ, creating it.
func testAccount(s *Store, typ string) (string, error) {
	id := "acc_" + typ
	_, err := s.sql.Exec(`INSERT INTO accounts (id, owner_id, type, label, source, env_key, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, 'env', ?, 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
		ON CONFLICT DO NOTHING`, id, DefaultOwner, typ, typ, "test-"+typ)
	return id, err
}

// A target that names no account is refused: picking one by type could
// send with credentials the post didn't name.
func TestSavePostRefusesTargetWithoutAccount(t *testing.T) {
	s := openTestStore(t)
	err := s.SavePost(&Post{ID: "p", Platforms: []string{"nostr"}, Status: "sending",
		Targets: []Target{{Platform: "nostr", Status: "sending"}}})
	if err == nil || !strings.Contains(err.Error(), "names no account") {
		t.Fatalf("SavePost = %v", err)
	}
	if _, err := s.GetPost("p"); err == nil {
		t.Error("the post was saved")
	}
	if n, _ := s.Accounts(context.Background(), DefaultOwner); len(n) != 0 {
		t.Errorf("accounts were made up: %+v", n)
	}
}

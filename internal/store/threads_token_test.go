package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestThreadsTokenSingletonAndJSON(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Absent → (nil, nil).
	if tk, err := db.GetThreadsToken(); err != nil || tk != nil {
		t.Fatalf("empty: tk=%v err=%v", tk, err)
	}

	exp := time.Now().Add(60 * 24 * time.Hour).UTC()
	if err := db.SaveThreadsToken("tok1", exp, "hashA", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tk, err := db.GetThreadsToken()
	if err != nil || tk == nil {
		t.Fatalf("get: %v %v", tk, err)
	}
	if tk.Token != "tok1" || tk.SeedHash != "hashA" {
		t.Errorf("got %+v", tk)
	}
	// Time columns must round-trip through the driver without drift.
	if !tk.ExpiresAt.Equal(exp) {
		t.Errorf("ExpiresAt round-trip: got %v want %v", tk.ExpiresAt, exp)
	}

	// Second save replaces the singleton row (id=1), not a second row.
	if err := db.SaveThreadsToken("tok2", exp, "hashB", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	tk, err = db.GetThreadsToken()
	if err != nil {
		t.Fatal(err)
	}
	if tk.Token != "tok2" || tk.SeedHash != "hashB" {
		t.Errorf("upsert failed: %+v", tk)
	}

	// Token must never serialize.
	b, _ := json.Marshal(tk)
	if strings.Contains(string(b), "tok2") {
		t.Errorf("token leaked into JSON: %s", b)
	}
}

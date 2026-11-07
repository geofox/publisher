package localauth

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func TestCheckPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"":                        false,
		"elevenchars":             false,
		"twelve chars":            true,
		"ééééééééééé":             false, // 11 characters, 22 bytes
		"éééééééééééé":            true,
		strings.Repeat("a", 1024): true,
		strings.Repeat("a", 1025): false,
		"bad \xff utf-8 text":     false,
	} {
		if err := CheckPassword(pw); (err == nil) != ok {
			t.Errorf("%q: %v", pw, err)
		}
	}
}

func TestHashAndVerify(t *testing.T) {
	h := NewHasher()
	phc, err := h.Hash("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=65536,t=3,p=4$") {
		t.Fatalf("phc %s", phc)
	}
	if other, _ := h.Hash("correct horse battery"); other == phc {
		t.Fatal("no salt")
	}
	if ok, err := h.Verify(phc, "correct horse battery"); err != nil || !ok {
		t.Fatalf("right password: %v %v", ok, err)
	}
	if ok, err := h.Verify(phc, "correct horse batterY"); err != nil || ok {
		t.Fatalf("wrong password: %v %v", ok, err)
	}
	if ok, _ := h.Verify(phc, strings.Repeat("a", 2000)); ok {
		t.Fatal("an oversized password matched")
	}
	for _, bad := range []string{"", "$2a$10$abc", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$a2V5",
		"$argon2id$v=19$m=9999999,t=3,p=4$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2i$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5"} {
		if _, err := h.Verify(bad, "x"); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// At most two hashes run at once; a third is refused at once.
func TestHasherLimit(t *testing.T) {
	h := NewHasher()
	h.sem <- struct{}{}
	h.sem <- struct{}{}
	if _, err := h.Hash("twelve chars"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third hash: %v", err)
	}
	if _, err := h.Verify("$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0$a2V5a2V5a2V5a2V5a2V5a2V5", "x"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third verify: %v", err)
	}
	<-h.sem
	<-h.sem
	var wg sync.WaitGroup
	var mu sync.Mutex
	busy, done := 0, 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.Hash("twelve chars")
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, ErrBusy) {
				busy++
			} else if err == nil {
				done++
			}
		}()
	}
	wg.Wait()
	if done < 1 || busy+done != 6 {
		t.Fatalf("done %d busy %d", done, busy)
	}
}

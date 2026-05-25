package nostr

import "testing"

func TestParseEventInput(t *testing.T) {
	hex := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		in      string
		wantHex string
	}{
		{hex, hex},
		{"nostr:" + hex, hex}, // tolerate a nostr: prefix on hex too
	}
	for _, c := range cases {
		ptr, err := parseEventInput(c.in)
		if err != nil {
			t.Fatalf("parseEventInput(%q): %v", c.in, err)
		}
		if ptr.IDHex != c.wantHex {
			t.Errorf("parseEventInput(%q).IDHex = %q, want %q", c.in, ptr.IDHex, c.wantHex)
		}
	}
}

func TestParseEventInputRejectsGarbage(t *testing.T) {
	if _, err := parseEventInput("not-an-id"); err == nil {
		t.Fatal("expected error for garbage input")
	}
}

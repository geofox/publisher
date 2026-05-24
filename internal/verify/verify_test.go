package verify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestVerdictJSONOmitsEmpty(t *testing.T) {
	v := Verdict{Platform: "nostr", Status: StatusVerified, Assurance: "cryptographic",
		Checks: []Check{{Name: "signature_valid", Result: "pass"}}}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"status":"verified"`) {
		t.Errorf("missing status: %s", s)
	}
	if strings.Contains(s, `"error"`) {
		t.Errorf("empty error should be omitted: %s", s)
	}
	if strings.Contains(s, `"signer"`) {
		t.Errorf("nil signer should be omitted: %s", s)
	}
}

func TestStatusConstants(t *testing.T) {
	if StatusVerified != "verified" || StatusFailed != "failed" || StatusError != "error" {
		t.Fatal("status constant values changed")
	}
}

func TestErrVerdictChecksNotNull(t *testing.T) {
	b, err := json.Marshal(errVerdict("bluesky", "network down"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	if !strings.Contains(s, `"checks":[]`) {
		t.Errorf("checks must serialize as [] not null: %s", s)
	}
	if !strings.Contains(s, `"status":"error"`) {
		t.Errorf("expected error status: %s", s)
	}
	if !strings.Contains(s, `"error":"network down"`) {
		t.Errorf("expected error message: %s", s)
	}
}

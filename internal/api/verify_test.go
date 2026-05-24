package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/geofox/publisher/internal/verify"
)

type stubVerifier struct{ v verify.Verdict }

func (s stubVerifier) Verify(_ context.Context, _ verify.Input) verify.Verdict { return s.v }

func postVerify(t *testing.T, a *API, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/verify", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	return rec
}

func TestHandleVerifyVerified(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "nostr", Status: verify.StatusVerified, Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out verify.Verdict
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Status != verify.StatusVerified {
		t.Errorf("status = %s", out.Status)
	}
}

func TestHandleVerifyFailedIs200(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "nostr", Status: verify.StatusFailed, Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("failed verdict should still be 200, got %d", rec.Code)
	}
}

func TestHandleVerifyErrorIs502(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{Platform: "bluesky", Status: verify.StatusError, Error: "network", Checks: []verify.Check{}}}}
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("error verdict should be 502, got %d", rec.Code)
	}
}

func TestHandleVerifyEmptyInputIs400(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{}}}
	rec := postVerify(t, a, `{"input":"  "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty input should be 400, got %d", rec.Code)
	}
}

func TestHandleVerifyNilVerifier503(t *testing.T) {
	a := &API{} // Verify is nil
	rec := postVerify(t, a, `{"input":"x"}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil verifier should be 503, got %d", rec.Code)
	}
}

func TestHandleVerifyMalformedJSON400(t *testing.T) {
	a := &API{Verify: stubVerifier{verify.Verdict{}}}
	rec := postVerify(t, a, `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed json should be 400, got %d", rec.Code)
	}
}

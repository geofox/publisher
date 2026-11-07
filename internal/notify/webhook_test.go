package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebhookAlert(t *testing.T) {
	var gotUser, gotPass string
	var okBasic bool
	var payload struct {
		Status string `json:"status"`
		Alerts []struct {
			Status       string            `json:"status"`
			Labels       map[string]string `json:"labels"`
			Annotations  map[string]string `json:"annotations"`
			GeneratorURL string            `json:"generatorURL"`
		} `json:"alerts"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, okBasic = r.BasicAuth()
		_ = json.NewDecoder(r.Body).Decode(&payload)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "alertmanager", "secret")
	wh.Source = "https://pub.example"
	if err := wh.Alert(context.Background(), "Threads token", "expires soon"); err != nil {
		t.Fatal(err)
	}
	if !okBasic || gotUser != "alertmanager" || gotPass != "secret" {
		t.Errorf("basic auth = %q/%q ok=%v", gotUser, gotPass, okBasic)
	}
	if payload.Status != "firing" || len(payload.Alerts) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
	a := payload.Alerts[0]
	if a.Annotations["summary"] != "Threads token" || a.Annotations["description"] != "expires soon" {
		t.Errorf("annotations = %v", a.Annotations)
	}
	if a.Labels["severity"] != "warning" || a.Labels["alertname"] != "publisher-threads-token" {
		t.Errorf("labels = %v", a.Labels)
	}
	// This server's address, never another's.
	if a.GeneratorURL != "https://pub.example" {
		t.Errorf("generatorURL = %q", a.GeneratorURL)
	}
}

func TestWebhookAlertNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()
	wh := NewWebhook(srv.URL, "alertmanager", "secret")
	if err := wh.Alert(context.Background(), "s", "b"); err == nil {
		t.Error("expected error on non-2xx relay response")
	}
}

func TestWebhookNoOpWhenUnconfigured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("must not POST when URL unconfigured")
	}))
	defer srv.Close()
	wh := NewWebhook("", "alertmanager", "secret")
	if err := wh.Alert(context.Background(), "s", "b"); err != nil {
		t.Errorf("unconfigured Alert should be a no-op, got %v", err)
	}
}

// An alert often carries an upstream error: its credentials are scrubbed
// before it leaves, and a failing relay's answer is shortened.
func TestWebhookAlertScrubs(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>" + strings.Repeat("x", 5000) + "</html>"))
	}))
	defer srv.Close()
	err := NewWebhook(srv.URL, "", "").Alert(context.Background(), "token for nsec1vl029mgpspedva04g90vltkh6fvh240zqtv9k0t9af8935ke9laqsnlfe5",
		`refresh: Get "https://graph.example.com/x?access_token=THAASECRET": timeout`)
	if strings.Contains(string(raw), "THAASECRET") || strings.Contains(string(raw), "vl029mgpspedva04") || !strings.Contains(string(raw), "timeout") {
		t.Errorf("sent %s", raw)
	}
	if err == nil || strings.Contains(err.Error(), "xxxx") {
		t.Errorf("err = %v", err)
	}
}

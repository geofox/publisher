package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/geofox/publisher/internal/httpx"
	"github.com/geofox/publisher/internal/translate"
)

// ─── GET /api/config ─────────────────────────────────────────────────────
//
// Operator preferences the frontend needs on boot. Currently just the spoken-
// languages list (drives the Bluesky/Mastodon language defaults + dropdown).
// Always returns an array (possibly empty) so the JS can treat it uniformly.

func (a *API) handleConfig(w http.ResponseWriter, _ *http.Request) {
	langs := a.UserLanguages
	if langs == nil {
		langs = []string{}
	}
	// translate_targets = user_languages ∩ DeepL-supported, preserving the
	// operator's ordering. Empty when no Translator is configured (the UI
	// hides the Translate button in that case).
	targets := []string{}
	if a.Translator != nil {
		targets = translate.Intersect(langs)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user_languages":    langs,
		"translate_targets": targets,
	})
}

// ─── GET /api/identity ───────────────────────────────────────────────────
//
// The operator's own cross-platform profile (handle, display name, avatar) so
// the composer can show real account data. Best-effort + cached; an unconfigured
// service returns {"accounts":{}} so the UI keeps its placeholders.

func (a *API) handleIdentity(w http.ResponseWriter, r *http.Request) {
	if a.Identity == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"accounts": map[string]any{}})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, a.Identity.Get(r.Context()))
}

// ─── POST /api/translate ─────────────────────────────────────────────────
//
// Body: {"text":"...","target_lang":"fr"} where target_lang is ISO 639-1.
// Returns {"text":"...","detected_source_language":"en"}. 503 when no
// Translator is configured (i.e. DEEPL_API_KEY unset).

type translateReq struct {
	Text       string `json:"text"`
	TargetLang string `json:"target_lang"`
}

type translateResp struct {
	Text                   string `json:"text"`
	DetectedSourceLanguage string `json:"detected_source_language"`
}

func (a *API) handleTranslate(w http.ResponseWriter, r *http.Request) {
	if a.Translator == nil {
		httpx.WriteError(w, http.StatusServiceUnavailable, "translation is not configured (DEEPL_API_KEY)")
		return
	}
	var req translateReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		httpx.WriteError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		httpx.WriteError(w, http.StatusBadRequest, "text is required")
		return
	}
	if !translate.IsSupported(req.TargetLang) {
		httpx.WriteError(w, http.StatusBadRequest, "unsupported target_lang: "+req.TargetLang)
		return
	}
	text, src, err := a.Translator.Translate(r.Context(), req.Text, req.TargetLang)
	if err != nil {
		httpx.WriteError(w, http.StatusBadGateway, err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, translateResp{Text: text, DetectedSourceLanguage: src})
}

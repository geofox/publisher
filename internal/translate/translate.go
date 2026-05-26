// Package translate proxies text-translation requests to DeepL. Free vs Pro
// tier is auto-detected from the API-key suffix (":fx" → Free), so the operator
// only configures DEEPL_API_KEY. The package also exposes the (small, stable)
// set of ISO 639-1 target codes DeepL accepts, so the API layer can intersect
// it with USER_LANGUAGES to drive the UI dropdown.
package translate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// deepLTargets maps ISO 639-1 (lowercase, what we exchange with the rest of the
// app) to DeepL's wire-format target_lang value. Where DeepL requires a
// region-specific code (PT-PT is the documented default since plain "PT" was
// deprecated), we pin a sensible default here.
var deepLTargets = map[string]string{
	"bg": "BG", "cs": "CS", "da": "DA", "de": "DE", "el": "EL", "en": "EN", "es": "ES", "et": "ET",
	"fi": "FI", "fr": "FR", "hu": "HU", "id": "ID", "it": "IT", "ja": "JA", "ko": "KO", "lt": "LT", "lv": "LV",
	"nb": "NB", "nl": "NL", "pl": "PL", "pt": "PT-PT", "ro": "RO", "ru": "RU", "sk": "SK", "sl": "SL",
	"sv": "SV", "tr": "TR", "uk": "UK", "zh": "ZH",
}

// SupportedTargets returns the ISO 639-1 codes DeepL accepts as targets,
// sorted, suitable for intersecting with the operator's configured languages.
func SupportedTargets() []string {
	out := make([]string, 0, len(deepLTargets))
	for k := range deepLTargets {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsSupported reports whether the given ISO 639-1 code is one we map to DeepL.
func IsSupported(code string) bool {
	_, ok := deepLTargets[strings.ToLower(code)]
	return ok
}

// Intersect returns the ISO 639-1 codes from `userLanguages` that DeepL
// supports, preserving the operator's ordering (first stays first → that's
// what the UI will show as the dropdown's leading option).
func Intersect(userLanguages []string) []string {
	out := make([]string, 0, len(userLanguages))
	for _, c := range userLanguages {
		if IsSupported(c) {
			out = append(out, strings.ToLower(c))
		}
	}
	return out
}

// DeepLClient is the production translator backed by DeepL. Construct via
// NewDeepL (auto-detects Free vs Pro from the key) or NewDeepLWithBase (tests).
type DeepLClient struct {
	apiKey  string
	baseURL string
	http    *http.Client
}

// NewDeepL returns a translator that talks to api-free.deepl.com when the key
// ends with ":fx" (DeepL Free), or api.deepl.com otherwise (Pro).
func NewDeepL(apiKey string) *DeepLClient {
	base := "https://api.deepl.com"
	if strings.HasSuffix(apiKey, ":fx") {
		base = "https://api-free.deepl.com"
	}
	return NewDeepLWithBase(apiKey, base)
}

// NewDeepLWithBase exposes the base URL for tests. baseURL has no trailing /v2.
func NewDeepLWithBase(apiKey, baseURL string) *DeepLClient {
	return &DeepLClient{
		apiKey:  apiKey,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Translate calls DeepL /v2/translate. target is ISO 639-1 (lowercase). Returns
// the translated text and DeepL's detected source language (also lowercase).
func (c *DeepLClient) Translate(ctx context.Context, text, target string) (string, string, error) {
	upTarget, ok := deepLTargets[strings.ToLower(target)]
	if !ok {
		return "", "", fmt.Errorf("translate: unsupported target %q", target)
	}
	if strings.TrimSpace(text) == "" {
		return "", "", errors.New("translate: empty text")
	}
	form := url.Values{"text": {text}, "target_lang": {upTarget}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v2/translate", strings.NewReader(form.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+c.apiKey)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("translate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", "", fmt.Errorf("deepl %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var out struct {
		Translations []struct {
			DetectedSourceLanguage string `json:"detected_source_language"`
			Text                   string `json:"text"`
		} `json:"translations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", fmt.Errorf("translate: decode: %w", err)
	}
	if len(out.Translations) == 0 {
		return "", "", errors.New("deepl: empty translations")
	}
	return out.Translations[0].Text, strings.ToLower(out.Translations[0].DetectedSourceLanguage), nil
}

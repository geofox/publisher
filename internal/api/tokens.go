package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/geofox/publisher/internal/httpx"
)

type tokenView struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	Revoked    bool   `json:"revoked"`
}

func iso(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func (a *API) handleListTokens(w http.ResponseWriter, _ *http.Request) {
	toks, err := a.Store.ListAPITokens()
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "list failed")
		return
	}
	out := make([]tokenView, 0, len(toks))
	for _, t := range toks {
		out = append(out, tokenView{
			ID: t.ID, Name: t.Name, CreatedAt: iso(t.CreatedAt),
			LastUsedAt: iso(t.LastUsedAt), Revoked: t.Revoked,
		})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

func (a *API) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil || body.Name == "" {
		httpx.WriteError(w, http.StatusBadRequest, "name required")
		return
	}
	tok, raw, err := a.Store.CreateAPIToken(body.Name)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "create failed")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"id": tok.ID, "name": tok.Name, "token": raw,
	})
}

func (a *API) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	err := a.Store.RevokeAPIToken(id)
	if errors.Is(err, sql.ErrNoRows) {
		httpx.WriteError(w, http.StatusNotFound, "token not found")
		return
	}
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "revoke failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe returns the authenticated user for the SPA's identity chip.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	u, ok := userFromContext(r.Context())
	if !ok {
		httpx.WriteError(w, http.StatusUnauthorized, "unauthenticated")
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"subject": u.Subject, "email": u.Email, "name": u.Name,
	})
}

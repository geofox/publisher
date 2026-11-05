package api

import (
	"log/slog"
	"net/http"

	apidocs "github.com/geofox/publisher/docs/api"
)

// GET /v1/openapi.json: the OpenAPI 3.1 description, open to anyone (it
// holds no data, and clients generate code from it).
func (a *API) v1OpenAPI(w http.ResponseWriter, r *http.Request) {
	b, err := apidocs.OpenAPIJSON()
	if err != nil {
		slog.ErrorContext(r.Context(), "v1: openapi document", "err", err)
		problem(w, r, http.StatusInternalServerError, codeInternal, "the OpenAPI document does not parse")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

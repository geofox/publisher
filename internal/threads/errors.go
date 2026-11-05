package threads

import (
	"encoding/json"
	"errors"

	"github.com/geofox/publisher/internal/destination"
)

// graphError is the error body the Graph API returns, usually with HTTP 400:
// the meaning is in the code, not the status.
// https://developers.facebook.com/docs/graph-api/guides/error-handling
type graphError struct {
	Error struct {
		Code        int  `json:"code"`
		IsTransient bool `json:"is_transient"`
	} `json:"error"`
}

// classify reads the Graph API error code when the response carries one, and
// falls back to destination.Classify (HTTP status, transport) otherwise.
func classify(err error) error {
	e := destination.Classify(err)
	var he *destination.HTTPError
	if !errors.As(err, &he) {
		return e
	}
	var g graphError
	if json.Unmarshal([]byte(he.Body), &g) != nil || g.Error.Code == 0 {
		return e
	}
	if c := classForGraph(g.Error.Code, g.Error.IsTransient); c != "" {
		e.Class = c
	}
	return e
}

// classForGraph maps a Graph API error code to a class, or "" to keep the
// status-based one.
func classForGraph(code int, transient bool) destination.ErrorClass {
	switch {
	case code == 4 || code == 17 || code == 32 || code == 613 || code == 341:
		return destination.ClassRateLimited // throttled: wait, then retry
	case code == 190 || code == 102 || code == 10 || (code >= 200 && code <= 299):
		return destination.ClassAuth // expired/invalid token or missing permission
	case transient || code == 1 || code == 2:
		return destination.ClassTransient // temporary server-side problem
	}
	return ""
}

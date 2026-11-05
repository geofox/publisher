package api

import (
	"encoding/json"
	"net/http"
)

// Problem details (RFC 9457) for /v1. Each error has a stable code; its
// type URI points at the code's entry in docs/api/errors.md, which
// TestV1ErrorCatalogue keeps in step with v1Codes.

// problemTypeBase prefixes a code to make the problem's type URI.
const problemTypeBase = "https://github.com/geofox/publisher/blob/main/docs/api/errors.md#"

// Error codes.
const (
	codeNotFound             = "not_found"
	codeMethodNotAllowed     = "method_not_allowed"
	codeUnauthenticated      = "unauthenticated"
	codeInsufficientScope    = "insufficient_scope"
	codeCrossOrigin          = "cross_origin"
	codeInvalidJSON          = "invalid_json"
	codeUnsupportedMediaType = "unsupported_media_type"
	codeTooLarge             = "too_large"
	codeValidation           = "validation_failed"
	codeInvalidParameter     = "invalid_parameter"
	codeInvalidCursor        = "invalid_cursor"
	codeInvalidState         = "invalid_state"
	codeIdemInvalid          = "idempotency_key_invalid"
	codeIdemInUse            = "idempotency_key_in_use"
	codeIdemReused           = "idempotency_key_reused"
	codeUnreadableUpload     = "unreadable_upload"
	codeUnavailable          = "unavailable"
	codeUpstream             = "upstream_failed"
	codeNoSpace              = "insufficient_storage"
	codeInternal             = "internal"
)

// v1Codes maps each code to its problem title, the same for every
// occurrence (RFC 9457 §3.1.3).
var v1Codes = map[string]string{
	codeNotFound:             "Not found",
	codeMethodNotAllowed:     "Method not allowed",
	codeUnauthenticated:      "Authentication required",
	codeInsufficientScope:    "Insufficient scope",
	codeCrossOrigin:          "Cross-origin request blocked",
	codeInvalidJSON:          "Invalid JSON body",
	codeUnsupportedMediaType: "Unsupported media type",
	codeTooLarge:             "Request body too large",
	codeValidation:           "Validation failed",
	codeInvalidParameter:     "Invalid query parameter",
	codeInvalidCursor:        "Invalid cursor",
	codeInvalidState:         "Not possible in the resource's current state",
	codeIdemInvalid:          "Invalid Idempotency-Key",
	codeIdemInUse:            "Idempotency-Key in use",
	codeIdemReused:           "Idempotency-Key reused",
	codeUnreadableUpload:     "Upload could not be read",
	codeUnavailable:          "Service unavailable",
	codeUpstream:             "Upstream service failed",
	codeNoSpace:              "Insufficient storage",
	codeInternal:             "Internal error",
}

// fieldError is one problem with one part of a request: a JSON Pointer
// (RFC 6901) into the body, or a query parameter's name.
type fieldError struct {
	Pointer   string `json:"pointer,omitempty"`
	Parameter string `json:"parameter,omitempty"`
	Code      string `json:"code"`
	Detail    string `json:"detail"`
}

// problemBody is an RFC 9457 problem with our code and errors extensions.
type problemBody struct {
	Type     string       `json:"type"`
	Title    string       `json:"title"`
	Status   int          `json:"status"`
	Detail   string       `json:"detail,omitempty"`
	Instance string       `json:"instance,omitempty"`
	Code     string       `json:"code"`
	Errors   []fieldError `json:"errors,omitempty"`
}

// problem answers an error as application/problem+json.
func problem(w http.ResponseWriter, r *http.Request, status int, code, detail string, errs ...fieldError) {
	title, ok := v1Codes[code]
	if !ok {
		// A code missing from the catalogue is a programming error; answer
		// it as internal rather than invent a type URI nobody documents.
		code, title = codeInternal, v1Codes[codeInternal]
	}
	b, _ := json.Marshal(problemBody{
		Type: problemTypeBase + code, Title: title, Status: status, Detail: detail,
		Instance: r.URL.Path, Code: code, Errors: errs,
	})
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// invalid answers a 422 listing every field error found.
func invalid(w http.ResponseWriter, r *http.Request, errs []fieldError) {
	detail := "the request has 1 invalid field"
	if len(errs) != 1 {
		detail = "the request has invalid fields"
	}
	problem(w, r, http.StatusUnprocessableEntity, codeValidation, detail, errs...)
}

package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
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
	codeMisdirected          = "misdirected"
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
	codePreconditionFailed   = "precondition_failed"
	codeUnreadableUpload     = "unreadable_upload"
	codeUnavailable          = "unavailable"
	codeUpstream             = "upstream_failed"
	codeNoSpace              = "insufficient_storage"
	codeInternal             = "internal"
	codeRateLimited          = "rate_limited"
	codeReauthRequired       = "reauth_required"
	codeSessionRequired      = "session_required"
	codeSignInRequired       = "sign_in_required"
	codeLastSignInMethod     = "last_sign_in_method"
	codeGone                 = "gone"
	codeCredentialsRejected  = "credentials_rejected"
	codeDuplicateAccount     = "duplicate_account"
	codeLockedByEnv          = "locked_by_env"
	codeIdentityChanged      = "identity_changed"
	codeMasterKeyRequired    = "master_key_required"
	codeMasterKeyMismatch    = "master_key_mismatch"
)

// v1Codes maps each code to its problem title, the same for every
// occurrence (RFC 9457 §3.1.3).
var v1Codes = map[string]string{
	codeNotFound:             "Not found",
	codeMethodNotAllowed:     "Method not allowed",
	codeUnauthenticated:      "Authentication required",
	codeInsufficientScope:    "Insufficient scope",
	codeCrossOrigin:          "Cross-origin request blocked",
	codeMisdirected:          "Misdirected request",
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
	codePreconditionFailed:   "Precondition failed",
	codeUnreadableUpload:     "Upload could not be read",
	codeUnavailable:          "Service unavailable",
	codeUpstream:             "Upstream service failed",
	codeNoSpace:              "Insufficient storage",
	codeInternal:             "Internal error",
	codeRateLimited:          "Too many requests",
	codeReauthRequired:       "Re-authentication required",
	codeSessionRequired:      "Browser session required",
	codeSignInRequired:       "Sign-in required",
	codeLastSignInMethod:     "Last sign-in method",
	codeGone:                 "Gone",
	codeCredentialsRejected:  "Credentials rejected",
	codeDuplicateAccount:     "Duplicate account",
	codeLockedByEnv:          "Set by environment variables",
	codeIdentityChanged:      "Another account",
	codeMasterKeyRequired:    "Master key required",
	codeMasterKeyMismatch:    "Master key mismatch",
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
	// Operation names what to re-authenticate for (reauth_required).
	Operation string `json:"operation,omitempty"`
	// Target names what the operation acts on, for an operation on one
	// account (reauth_required): re-authenticate for it.
	Target string `json:"target,omitempty"`
	// AccountID names the account a duplicate_account is the same as.
	AccountID string `json:"account_id,omitempty"`
}

// problem answers an error as application/problem+json.
func problem(w http.ResponseWriter, r *http.Request, status int, code, detail string, errs ...fieldError) {
	writeProblem(w, r, problemBody{Status: status, Code: code, Detail: detail, Errors: errs})
}

// writeProblem answers p, filling its type, title and instance.
func writeProblem(w http.ResponseWriter, r *http.Request, p problemBody) {
	title, ok := v1Codes[p.Code]
	if !ok {
		// A code missing from the catalogue is a programming error; answer
		// it as internal rather than invent a type URI nobody documents.
		p.Code, title = codeInternal, v1Codes[codeInternal]
	}
	p.Type, p.Title, p.Instance = problemTypeBase+p.Code, title, r.URL.Path
	b, _ := json.Marshal(p)
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_, _ = w.Write(b)
}

// rateLimited answers 429 rate_limited with Retry-After in whole seconds.
func rateLimited(w http.ResponseWriter, r *http.Request, wait time.Duration, detail string) {
	secs := int64((wait + time.Second - 1) / time.Second)
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	problem(w, r, http.StatusTooManyRequests, codeRateLimited, detail)
}

// invalid answers a 422 listing every field error found.
func invalid(w http.ResponseWriter, r *http.Request, errs []fieldError) {
	detail := "the request has 1 invalid field"
	if len(errs) != 1 {
		detail = "the request has invalid fields"
	}
	problem(w, r, http.StatusUnprocessableEntity, codeValidation, detail, errs...)
}

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/geofox/publisher/internal/store"
)

// Idempotency-Key (draft-ietf-httpapi-idempotency-key-header-08) for /v1
// creates. A request carrying a key runs once per principal and key: a
// repeat with the same payload gets the stored response, marked
// Idempotent-Replayed: true; a repeat with another payload (or another
// route) gets 422; a repeat while the first is still running gets 409.
// Keys last store.IdempotencyTTL. Responses that say "try again" (5xx,
// 409, 429) are not stored, so the client can retry with the same key.

// maxIdempotencyKey bounds a key's length.
const maxIdempotencyKey = 255

// replayedHeaders are the response headers stored with a response and
// replayed with it. The rest (security headers, Date) are set afresh.
var replayedHeaders = []string{"Content-Type", "Location", "ETag", "Preference-Applied"}

// parseIdempotencyKey reads the header's value: a Structured Fields string
// (RFC 8941 §3.3.3, as the draft specifies) or, for hand-written requests,
// a bare value of visible ASCII.
func parseIdempotencyKey(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		var b strings.Builder
		for i := 1; i < len(v); i++ {
			c := v[i]
			switch {
			case c == '\\':
				i++
				if i >= len(v) || (v[i] != '"' && v[i] != '\\') {
					return "", false
				}
				b.WriteByte(v[i])
			case c == '"':
				if i != len(v)-1 {
					return "", false
				}
				k := b.String()
				if k == "" || len(k) > maxIdempotencyKey {
					return "", false
				}
				return k, true
			case c < 0x20 || c > 0x7e:
				return "", false
			default:
				b.WriteByte(c)
			}
		}
		return "", false // unterminated
	}
	if v == "" || len(v) > maxIdempotencyKey {
		return "", false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; c <= 0x20 || c > 0x7e || c == '"' || c == '\\' || c == ',' || c == ';' {
			return "", false
		}
	}
	return v, true
}

// withIdempotency applies Idempotency-Key to a create. It runs inside
// v1Auth, so the principal is known.
func (a *API) withIdempotency(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, sent := r.Header["Idempotency-Key"]
		if !sent {
			next.ServeHTTP(w, r)
			return
		}
		key, ok := "", len(raw) == 1
		if ok {
			key, ok = parseIdempotencyKey(raw[0])
		}
		if !ok {
			problem(w, r, http.StatusBadRequest, codeIdemInvalid,
				`Idempotency-Key must be one quoted string of 1 to 255 visible ASCII characters, such as "6f1c2b"`)
			return
		}
		p, ok := principalFrom(r.Context())
		if !ok || a.Store == nil {
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "no database")
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxV1Body))
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				problem(w, r, http.StatusRequestEntityTooLarge, codeTooLarge, "the body is over 1 MiB")
				return
			}
			problem(w, r, http.StatusBadRequest, codeInvalidJSON, "the body could not be read")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.New()
		sum.Write([]byte(r.Header.Get("Content-Type")))
		sum.Write([]byte{0})
		sum.Write(body)
		fp := hex.EncodeToString(sum.Sum(nil))

		ctx := context.WithoutCancel(r.Context())
		who := p.key()
		prev, err := a.Store.BeginIdempotent(ctx, who, key, r.Method, r.URL.Path, fp, time.Now())
		switch {
		case errors.Is(err, store.ErrIdempotencyBusy):
			w.Header().Set("Retry-After", "1")
			problem(w, r, http.StatusConflict, codeIdemInUse, "a request with this Idempotency-Key is still being processed")
			return
		case err != nil:
			slog.ErrorContext(ctx, "v1: idempotency key", "err", err)
			problem(w, r, http.StatusServiceUnavailable, codeUnavailable, "the idempotency store is unavailable")
			return
		case prev != nil:
			if prev.Method != r.Method || prev.Path != r.URL.Path || prev.Print != fp {
				problem(w, r, http.StatusUnprocessableEntity, codeIdemReused,
					"this Idempotency-Key was used for a different request; use a new key for a new request")
				return
			}
			for _, h := range replayedHeaders {
				if v := prev.Header.Get(h); v != "" {
					w.Header().Set(h, v)
				}
			}
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(prev.Status)
			_, _ = w.Write(prev.Body)
			return
		}

		rec := &recorder{ResponseWriter: w}
		finished := false
		defer func() {
			if finished {
				return
			}
			// The handler panicked: free the key, then let v1Recover answer.
			if err := a.Store.AbandonIdempotent(ctx, who, key); err != nil {
				slog.ErrorContext(ctx, "v1: abandon idempotency key", "err", err)
			}
		}()
		next.ServeHTTP(rec, r)
		finished = true
		status := rec.statusOr200()
		if status >= 500 || status == http.StatusConflict || status == http.StatusTooManyRequests {
			if err := a.Store.AbandonIdempotent(ctx, who, key); err != nil {
				slog.ErrorContext(ctx, "v1: abandon idempotency key", "err", err)
			}
			return
		}
		hdr := http.Header{}
		for _, h := range replayedHeaders {
			if v := rec.Header().Get(h); v != "" {
				hdr.Set(h, v)
			}
		}
		if err := a.Store.FinishIdempotent(ctx, who, key, status, hdr, rec.body.Bytes()); err != nil {
			// The response went out; only a repeat will notice (as 409
			// until the claim goes stale).
			slog.ErrorContext(ctx, "v1: store idempotent response", "err", err)
		}
	})
}

// recorder passes a response through while keeping a copy.
type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *recorder) statusOr200() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

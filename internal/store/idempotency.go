package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// IdempotencyTTL is how long a stored response answers a repeated
// Idempotency-Key (draft-ietf-httpapi-idempotency-key-header asks for it to
// be documented).
const IdempotencyTTL = 24 * time.Hour

// IdempotencyStale is how long an in-flight claim holds its key: a request
// still unanswered after this died with its process.
const IdempotencyStale = 10 * time.Minute

// Idempotency key states.
const (
	IdemInFlight = "in_flight"
	IdemDone     = "done"
)

// IdempotentResponse is a stored response to replay.
type IdempotentResponse struct {
	Status  int
	Header  http.Header
	Body    []byte
	Method  string
	Path    string
	Print   string // the request's fingerprint
	State   string
	Created time.Time
}

// ErrIdempotencyBusy is returned by BeginIdempotent when an earlier request
// with the key is still in flight.
var ErrIdempotencyBusy = errors.New("idempotency key in use")

// BeginIdempotent claims key for principal's request (method, path,
// fingerprint). It returns nil when the request should run; or the stored
// record of an earlier request with the key, for the caller to compare and
// replay; or ErrIdempotencyBusy while that request is in flight. Expired
// records are replaced.
func (s *Store) BeginIdempotent(ctx context.Context, principal, key, method, path, fingerprint string, now time.Time) (*IdempotentResponse, error) {
	tx, err := s.sql.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	// An expired key is free again, and so is one whose request died with
	// its process (nothing holds a request that long).
	if _, err := tx.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE principal = ? AND key = ?
		AND (expires_at <= ? OR (state = ? AND created_at <= ?))`,
		principal, key, now.UTC(), IdemInFlight, now.Add(-IdempotencyStale).UTC()); err != nil {
		return nil, err
	}
	var rec IdempotentResponse
	var status sql.NullInt64
	var header sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT method, path, fingerprint, state, response_status, response_headers, response_body, created_at
		FROM idempotency_keys WHERE principal = ? AND key = ?`, principal, key).
		Scan(&rec.Method, &rec.Path, &rec.Print, &rec.State, &status, &header, &rec.Body, &rec.Created)
	switch {
	case err == nil:
		if rec.State == IdemInFlight && rec.Print == fingerprint && rec.Method == method && rec.Path == path {
			return nil, ErrIdempotencyBusy
		}
		rec.Status = int(status.Int64)
		if header.Valid {
			_ = json.Unmarshal([]byte(header.String), &rec.Header)
		}
		return &rec, nil
	case !errors.Is(err, sql.ErrNoRows):
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO idempotency_keys (principal, key, method, path, fingerprint, state, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, principal, key, method, path, fingerprint, IdemInFlight, now.UTC(), now.Add(IdempotencyTTL).UTC()); err != nil {
		return nil, err
	}
	return nil, tx.Commit()
}

// FinishIdempotent stores the response to the request that claimed key at
// claimed (the now it passed to BeginIdempotent). A claim that went stale
// and was taken over by a later request no longer matches, so a slow
// request can't overwrite its successor's record.
func (s *Store) FinishIdempotent(ctx context.Context, principal, key string, claimed time.Time, status int, header http.Header, body []byte) error {
	h, _ := json.Marshal(header)
	_, err := s.sql.ExecContext(ctx, `UPDATE idempotency_keys SET state = ?, response_status = ?, response_headers = ?, response_body = ?
		WHERE principal = ? AND key = ? AND state = ? AND created_at = ?`,
		IdemDone, status, string(h), body, principal, key, IdemInFlight, claimed.UTC())
	return err
}

// AbandonIdempotent forgets the claim made at claimed, whose request did
// not complete (a server error, a panic), so the client can try again.
func (s *Store) AbandonIdempotent(ctx context.Context, principal, key string, claimed time.Time) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE principal = ? AND key = ? AND state = ? AND created_at = ?`,
		principal, key, IdemInFlight, claimed.UTC())
	return err
}

// PurgeIdempotency deletes expired keys and returns how many.
func (s *Store) PurgeIdempotency(ctx context.Context, now time.Time) (int, error) {
	res, err := s.sql.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= ?`, now.UTC())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

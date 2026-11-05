package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Prefixed ids. Accounts carry their prefix in the store; tokens get it at
// the API boundary.
const prefixToken = "tok_"

// tokenID makes a token's API id.
func tokenID(storeID string) string { return prefixToken + storeID }

// parseID strips prefix from an API id; ok is false when it is missing or
// nothing follows it.
func parseID(id, prefix string) (string, bool) {
	rest, ok := strings.CutPrefix(id, prefix)
	return rest, ok && rest != ""
}

// Pagination: ?limit=&cursor=, answered as {"data": [...], "next_cursor"}.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// cursor is the position after the last item of a page: its sort time and
// id. Keyset positions stay correct when items are added before them.
type cursor struct {
	Time time.Time `json:"t"`
	ID   string    `json:"i"`
}

func (c cursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (cursor, bool) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, false
	}
	var c cursor
	if json.Unmarshal(b, &c) != nil || c.ID == "" {
		return cursor{}, false
	}
	return c, true
}

// pageParams reads ?limit= and ?cursor=; on a bad value it answers a
// problem and returns false.
func pageParams(w http.ResponseWriter, r *http.Request) (limit int, after *cursor, ok bool) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxPageLimit {
			problem(w, r, http.StatusBadRequest, codeInvalidParameter, "limit must be a whole number from 1 to 200",
				fieldError{Parameter: "limit", Code: "out_of_range", Detail: "from 1 to 200"})
			return 0, nil, false
		}
		limit = n
	}
	if s := q.Get("cursor"); s != "" {
		c, valid := decodeCursor(s)
		if !valid {
			problem(w, r, http.StatusBadRequest, codeInvalidCursor, "cursor is not one this server issued",
				fieldError{Parameter: "cursor", Code: "invalid", Detail: "pass next_cursor from the previous page unchanged"})
			return 0, nil, false
		}
		after = &c
	}
	return limit, after, true
}

// pageOf returns the page of items (already sorted by key, newest first
// when desc) that follows after, and the cursor for the next page (nil on
// the last page).
func pageOf[T any](items []T, key func(T) cursor, desc bool, after *cursor, limit int) ([]T, *cursor) {
	start := 0
	if after != nil {
		start = len(items)
		for i, it := range items {
			if cursorAfter(key(it), *after, desc) {
				start = i
				break
			}
		}
	}
	end := min(start+limit, len(items))
	out := items[start:end]
	if end == len(items) || len(out) == 0 {
		return out, nil
	}
	next := key(out[len(out)-1])
	return out, &next
}

// cursorAfter reports whether k sorts strictly after c in the list order
// (time, then id; both descending when desc).
func cursorAfter(k, c cursor, desc bool) bool {
	cmp := k.Time.Compare(c.Time)
	if cmp == 0 {
		cmp = strings.Compare(k.ID, c.ID)
	}
	if desc {
		return cmp < 0
	}
	return cmp > 0
}

// listBody is a page of a collection.
type listBody[T any] struct {
	Data       []T     `json:"data"`
	NextCursor *string `json:"next_cursor"`
}

func newList[T any](data []T, next *cursor) listBody[T] {
	if data == nil {
		data = []T{}
	}
	b := listBody[T]{Data: data}
	if next != nil {
		s := next.encode()
		b.NextCursor = &s
	}
	return b
}

// rfc3339 formats a timestamp for /v1: RFC 3339, UTC, seconds.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// rfc3339p is rfc3339 for an optional timestamp: nil when zero.
func rfc3339p(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := rfc3339(t)
	return &s
}

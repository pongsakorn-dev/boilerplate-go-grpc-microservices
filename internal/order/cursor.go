package order

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"time"
)

// Keyset pagination, not OFFSET.
//
// OFFSET pagination is wrong in a way that is easy to miss in tests and obvious in
// production: if a row is inserted or deleted between page 1 and page 2, OFFSET silently
// skips or repeats rows. It also degrades linearly, because the database still has to
// walk and discard every skipped row.
//
// The token is opaque ON PURPOSE. It is base64url of a JSON blob here because that is
// readable while you are learning the template; the contract with clients is only that
// they pass it back unmodified. Changing the encoding later is not a breaking API change
// precisely because nothing is promised about its contents.
//
// Retrofitting keyset pagination onto a shipped API is a breaking change plus an index
// change, which is why it is in the template from day one rather than deferred.
type cursor struct {
	// CreatedAt and ID together form the sort key. ID breaks ties so the ordering is
	// total -- without it, rows sharing a timestamp can be skipped or repeated.
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"i"`

	// FilterHash pins the filter the token was issued for. Paging with a token from a
	// different filter would otherwise return a silently wrong result set.
	FilterHash uint32 `json:"h"`
}

func encodeCursor(c cursor) (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("encode page token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func decodeCursor(token string, wantHash uint32) (cursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: not valid base64url", ErrInvalidPageToken)
	}
	var c cursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return cursor{}, fmt.Errorf("%w: malformed payload", ErrInvalidPageToken)
	}
	if c.FilterHash != wantHash {
		return cursor{}, fmt.Errorf("%w: token was issued for a different filter", ErrInvalidPageToken)
	}
	return c, nil
}

// filterHash covers every field that changes the result set. Adding a filter field
// without adding it here would let a stale token silently page through the wrong rows.
func filterHash(tenantID string, f ListFilter) uint32 {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%s\x00%d\x00%s", tenantID, f.Status, f.CustomerID)
	return h.Sum32()
}

// EncodePageToken builds the token for the last row of a page. It is exported so store
// adapters can produce tokens without duplicating the encoding.
func EncodePageToken(tenantID string, f ListFilter, last Order) (string, error) {
	return encodeCursor(cursor{
		CreatedAt:  last.CreatedAt.UTC(),
		ID:         last.ID,
		FilterHash: filterHash(tenantID, f),
	})
}

// DecodePageToken returns the sort key to resume after. An empty token means "start at
// the beginning", which is not an error.
func DecodePageToken(tenantID string, f ListFilter) (after time.Time, afterID string, ok bool, err error) {
	if f.PageToken == "" {
		return time.Time{}, "", false, nil
	}
	c, err := decodeCursor(f.PageToken, filterHash(tenantID, f))
	if err != nil {
		return time.Time{}, "", false, err
	}
	return c.CreatedAt, c.ID, true, nil
}

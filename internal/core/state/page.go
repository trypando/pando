package state

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/trypando/pando/internal/errs"
)

// Keyset pagination for the lists that grow with the organization: accounts,
// groups, apps and waiting approvals (issue #72). Design 04 §1: `?limit=` and
// `?cursor=`, and a response carrying `next_cursor`.
//
// A cursor is the sort key of the last row of the previous page, encoded so a
// client treats it as opaque. Keyset rather than OFFSET: an offset reads and
// throws away every earlier row on each page, and rows created between two
// requests shift every later page by one.

// Page sizes. The default is a screenful; the maximum bounds what one request
// may read, whatever it asks for.
const (
	DefaultPageSize = 100
	MaxPageSize     = 500
)

// TotalCap is how far a list's total is counted exactly (O-53). Counting
// every match costs a read of every match, which at 100,000 accounts is the
// whole table on every page; a count that stops past the cap costs at most
// the cap. A list holding more reports TotalCap+1, which the API sends as
// TotalCap with `total_is_lower_bound` ("10,000+").
const TotalCap = 10000

// countCapped counts the rows `from` (a FROM clause and its WHERE) selects,
// stopping at TotalCap+1.
func countCapped(ctx context.Context, db *DB, from string, args ...any) (int, error) {
	var n int
	err := db.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 %s LIMIT %d) capped`, from, TotalCap+1),
		args...).Scan(&n)
	return n, err
}

// Page asks for one page of a list.
type Page struct {
	// Limit is the most rows to return. Zero means DefaultPageSize; anything
	// over MaxPageSize is read as MaxPageSize.
	Limit int
	// Cursor is the next_cursor of the previous page, or empty for the first.
	Cursor string
	// Query narrows the list by a case-insensitive substring match, on
	// whatever a person would recognize the row by (a name, an email).
	Query string
	// IDs, when set, narrows the list to these rows: how a client refreshes
	// the few rows it is watching change without reading the whole list again.
	// The app and account lists read it.
	IDs []string
}

// Size is the number of rows the page holds at most.
func (p Page) Size() int {
	switch {
	case p.Limit <= 0:
		return DefaultPageSize
	case p.Limit > MaxPageSize:
		return MaxPageSize
	}
	return p.Limit
}

// ErrBadCursor refuses a cursor that did not come from a previous page.
var ErrBadCursor = errs.New(errs.ValidInvalid,
	"The cursor must be the next_cursor value from a previous page of the same list.").
	WithRemedy("Leave the cursor out to start from the first page.")

// encodeCursor turns a row's sort key into a cursor.
func encodeCursor(key ...any) string {
	b, _ := json.Marshal(key)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor reads a cursor into the given pointers, in order. An empty
// cursor reports false and leaves them alone.
func decodeCursor(cursor string, into ...any) (bool, error) {
	if cursor == "" {
		return false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return false, ErrBadCursor
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts) != len(into) {
		return false, ErrBadCursor
	}
	for i, target := range into {
		if err := json.Unmarshal(parts[i], target); err != nil {
			return false, ErrBadCursor
		}
	}
	return true, nil
}

package httpapi

import (
	"net/http"
	"strconv"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// withTotal adds a list's total to its response body as the API sends it
// (O-53): exact up to state.TotalCap, and past it the cap with
// `total_is_lower_bound` set — "10,000+" — because counting every match costs
// a read of every match on every page.
func withTotal(body map[string]any, total int) map[string]any {
	body["total"] = min(total, state.TotalCap)
	body["total_is_lower_bound"] = total > state.TotalCap
	return body
}

// pageFrom reads a list's page from the query string: `limit`, `cursor` and
// `q` (design 04 §1). Reading only; what a page holds is the state layer's.
func pageFrom(r *http.Request) (state.Page, error) {
	q := r.URL.Query()
	page := state.Page{Cursor: q.Get("cursor"), Query: q.Get("q")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return state.Page{}, errs.Newf(errs.ValidInvalid,
				"The limit must be a whole number of records from 1 to %d.", state.MaxPageSize)
		}
		page.Limit = n
	}
	if ids := q["id"]; len(ids) > 0 {
		if len(ids) > state.MaxPageSize {
			return state.Page{}, errs.Newf(errs.ValidInvalid,
				"A list can be narrowed to at most %d ids at once.", state.MaxPageSize)
		}
		page.IDs = ids
	}
	return page, nil
}

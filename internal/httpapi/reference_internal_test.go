package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/authz"
)

// TestR261_EveryRouteIsDocumented is the mechanism that keeps the reference
// true.
//
// R-261 makes the API the product. A description of a product that drifts from
// it is worse than none: it is trusted, and it is wrong in exactly the places
// somebody automated against. chi knows every route this binary serves, so the
// table in reference.go is checked against the router rather than against
// somebody's memory — a new endpoint with no summary fails here, and so does a
// summary for an endpoint that has been removed.
//
// This runs in `make check`, which is the point. The rule is not "remember to
// update the docs"; the rule is that the build does not pass until they are.
func TestR261_EveryRouteIsDocumented(t *testing.T) {
	handler := (&Server{Logger: zap.NewNop()}).Routes()
	routes, ok := handler.(chi.Routes)
	require.True(t, ok, "the router must be walkable for this check to mean anything")

	served := map[string]bool{}
	err := chi.Walk(routes, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			// Health, JWKS, the console's own paths and the reserved prefix's
			// re-entrant mount. None of them is an API a client calls, and the
			// reserved mount reports every method against one pattern.
			return nil
		}
		served[method+" "+normalizeRoutePath(route)] = true
		return nil
	})
	require.NoError(t, err)
	require.NotEmpty(t, served, "walking the router found nothing, so this test proves nothing")

	documented := map[string]bool{}
	for _, doc := range routeDocs {
		key := doc.Method + " " + normalizeRoutePath(doc.Path)
		require.False(t, documented[key], "%s is documented twice", key)
		documented[key] = true

		require.NotEmpty(t, doc.Summary, "%s has no summary", key)
		require.NotEmpty(t, doc.Group, "%s is in no group", key)
	}

	for key := range served {
		require.True(t, documented[key],
			"%s is served and not documented. Add it to routeDocs in reference.go — the console's "+
				"API screen and docs/api.md are built from that table.", key)
	}
	for key := range documented {
		require.True(t, served[key],
			"%s is documented and not served. Remove it from routeDocs in reference.go.", key)
	}
}

// The verb on a row is the one the handler checks. Nothing enforces that
// mechanically — a handler's verb is a call inside a function body, not
// something the router knows — so this asserts the weaker property that keeps
// the column honest: a verb named here is a verb that exists.
func TestDocumentedVerbsAreRealVerbs(t *testing.T) {
	known := map[string]bool{}
	for _, v := range authz.Verbs {
		known[string(v)] = true
	}

	for _, doc := range routeDocs {
		if doc.Verb == "" {
			continue
		}
		require.True(t, known[doc.Verb], "%s %s names %q, which is not a verb", doc.Method, doc.Path, doc.Verb)
	}
}

// TestTheReferenceIsBuiltOnceAndServedWhole asserts GET /reference builds its
// document once, however often it is asked for, and serves the same JSON the
// generator writes the docs from.
func TestTheReferenceIsBuiltOnceAndServedWhole(t *testing.T) {
	s := &Server{}
	serve := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleReference(w, httptest.NewRequest(http.MethodGet, "/api/v1/reference", nil))
		return w
	}
	first := serve()
	before := referenceBuilds.Load()
	second := serve()
	require.Equal(t, before, referenceBuilds.Load(), "the second request built nothing")
	require.Equal(t, int64(1), before)
	require.Equal(t, http.StatusOK, second.Code)
	require.Equal(t, "application/json; charset=utf-8", second.Header().Get("Content-Type"))
	require.Equal(t, first.Body.String(), second.Body.String())

	var doc struct {
		API struct {
			Routes []json.RawMessage `json:"routes"`
		} `json:"api"`
	}
	require.NoError(t, json.Unmarshal(second.Body.Bytes(), &doc))
	require.Len(t, doc.API.Routes, len(Reference().API.Routes))
}

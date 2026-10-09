package state

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Where each app is reached: apps.address_hostname and apps.address_path,
// written when a revision is pinned (migration 35).

// querier is what an address check reads through: the pool, or the pin's
// transaction.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// addressOf is the hostname and path a routing block claims. At most one is
// set; a port-mode app claims neither (its port is port_allocations').
func addressOf(r spec.Routing) (hostname, path string) {
	switch r.Mode {
	case spec.RoutingSubdomain:
		return strings.ToLower(r.Hostname), ""
	case spec.RoutingPath:
		return "", strings.ToLower(r.PathPrefix)
	}
	return "", ""
}

// portOf is the port a routing block claims as its address: a port-mode
// app's, and no other's. Zero means none.
func portOf(r spec.Routing) int {
	if r.Mode == spec.RoutingPort {
		return r.Port
	}
	return 0
}

// CheckAddress reports whether another live app already holds the address r
// would claim for appID, saying which address and why. nil means it is free.
//
// Asked before a change is saved, so the person choosing hears it then, and
// again inside Pin, which is where it is decided: the unique indexes refuse
// an exact duplicate there however two writers race, and this refuses the
// rest — a path inside or around another app's, or one that takes another
// app's slug.
func (a *Apps) CheckAddress(ctx context.Context, appID string, r spec.Routing) error {
	return checkAddress(ctx, a.db, appID, r)
}

func checkAddress(ctx context.Context, q querier, appID string, r spec.Routing) error {
	hostname, path := addressOf(r)

	if hostname != "" {
		var other string
		err := q.QueryRow(ctx, `
			SELECT name FROM apps
			WHERE deleted_at IS NULL AND id <> $1 AND address_hostname = $2
			LIMIT 1`, appID, hostname).Scan(&other)
		if err == nil {
			return taken(hostname, other, "is already reached at it")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return errs.Wrap(errs.Internal, "Could not check whether the address is free.", err)
		}
	}

	if path != "" {
		// The same path, or one inside or around it on a whole-segment
		// boundary: /team and /team/notes cannot both be apps, since one
		// would receive the other's requests, and an app claiming a path
		// under another's is a page in that app's name it does not control.
		var other, otherPath string
		err := q.QueryRow(ctx, `
			SELECT name, address_path FROM apps
			WHERE deleted_at IS NULL AND id <> $1 AND address_path IS NOT NULL
			  AND (address_path = $2
			       OR starts_with($2, address_path || '/')
			       OR starts_with(address_path, $2 || '/'))
			LIMIT 1`, appID, path).Scan(&other, &otherPath)
		if err == nil {
			switch {
			case otherPath == path:
				return taken(path, other, "is already reached at it")
			case strings.HasPrefix(path, otherPath+"/"):
				return taken(path, other, "is reached at "+otherPath+", which this path is inside")
			default:
				return taken(path, other, "is reached at "+otherPath+", which is inside this path")
			}
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return errs.Wrap(errs.Internal, "Could not check whether the address is free.", err)
		}

		// Another app's slug as the first segment. Every app answers at
		// /<slug> as well as its own path (proxy.resolve), and a path that
		// took one would take that app's requests.
		first := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0]
		err = q.QueryRow(ctx, `
			SELECT name FROM apps
			WHERE deleted_at IS NULL AND id <> $1 AND slug = $2
			LIMIT 1`, appID, first).Scan(&other)
		if err == nil {
			return taken(path, other, "is reached at /"+first)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return errs.Wrap(errs.Internal, "Could not check whether the address is free.", err)
		}
	}
	return nil
}

func taken(address, other, why string) error {
	return errs.Newf(errs.StateAddressTaken, "%s can't be this app's address: %s %s.", address, other, why).
		WithRemedy("Choose another address for this app, or change the other app's first.")
}

// ByPath resolves the path-mode app whose path is the longest prefix of the
// request's path, on whole segments, and returns that path.
//
// Case-sensitive, as a URL's path is: paths are stored lowercase, and the
// proxy strips exactly the prefix that matched.
//
// The candidates are built here and matched with = ANY, so the lookup is the
// unique index on address_path rather than a scan comparing the request's
// path with every app's. An app's path matches when it is the request's path,
// or the request's path continues past it with a slash — which is exactly
// when it is one of pathCandidates.
func (a *Apps) ByPath(ctx context.Context, requestPath string) (App, *spec.AppSpec, string, bool, error) {
	candidates := pathCandidates(requestPath)
	if len(candidates) == 0 {
		return App{}, nil, "", false, nil
	}
	// The app and its pinned spec with the match, in one query rather than a
	// second ByRouting by ID (issue #93). A LEFT JOIN, so the longest match
	// is chosen among every app as before, and one with no pinned spec is
	// "no app here" rather than a shorter path's app.
	var (
		app    App
		owner  *string
		pinned *string
		body   []byte
		prefix string
	)
	err := a.db.QueryRow(ctx, `
		SELECT a.id, a.name, a.slug, a.owner_user_id, a.state, a.desired_state, a.pinned_spec_id,
		       a.created_at, a.updated_at, r.body, a.address_path
		FROM apps a
		LEFT JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL AND a.address_path = ANY($1)
		ORDER BY length(a.address_path) DESC
		LIMIT 1`, candidates).
		Scan(&app.ID, &app.Name, &app.Slug, &owner, &app.State, &app.DesiredState, &pinned,
			&app.CreatedAt, &app.UpdatedAt, &body, &prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, nil, "", false, nil
	}
	if err != nil {
		return App{}, nil, "", false, errs.Wrap(errs.Internal, "Could not look up the app for this path.", err)
	}
	if pinned == nil || body == nil {
		return App{}, nil, "", false, nil
	}
	app.PinnedSpecID = *pinned
	if owner != nil {
		app.OwnerUserID = *owner
	}
	var s spec.AppSpec
	if err := json.Unmarshal(body, &s); err != nil {
		return App{}, nil, "", false, errs.Wrap(errs.Internal, "Could not read the app's spec.", err)
	}
	return app, &s, prefix, true, nil
}

// maxPathCandidates bounds how many prefixes of one request's path are looked
// up. An app's path has at most four segments (spec.CheckPathPrefix); this is
// far past that, and only stops a request with thousands of slashes in its
// path from becoming a query with thousands of parameters.
const maxPathCandidates = 64

// pathCandidates is every path an app could hold and receive this request
// under: the request's path, and each prefix of it that ends just before a
// slash. /a/b/c gives /a/b/c, /a and /a/b.
func pathCandidates(p string) []string {
	if p == "" {
		return nil
	}
	out := []string{p}
	for i := 1; i < len(p) && len(out) < maxPathCandidates; i++ {
		if p[i] == '/' {
			out = append(out, p[:i])
		}
	}
	return out
}

package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/id"
)

// Everything the seed writes is a pure function of the tier and an index, so
// a second run computes the same rows, finds them already there, and moves
// on: that is what makes seeding resumable. IDs included — they are real
// prefixed ULIDs (internal/id), with a fixed time and entropy drawn from the
// object's kind and index rather than from the clock and crypto/rand.

// seedEpoch is the time every seeded ID claims. Fixed, so IDs are stable
// across runs; distinct per index by a millisecond, so they still sort in
// creation order like any other ID.
var seedEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedID is a deterministic, well-formed ID of kind k for index i. salt keeps
// two different uses of one kind (a data grant and a control grant) apart.
func seedID(k id.Kind, salt string, i int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("pando-load/%s/%s/%d", k, salt, i)))
	ms := ulid.Timestamp(seedEpoch.Add(time.Duration(i) * time.Millisecond))
	u, err := ulid.New(ms, bytes.NewReader(sum[:10]))
	if err != nil {
		// ulid.New fails only on a short entropy reader or an out-of-range
		// time, and neither is possible here.
		panic(err)
	}
	return string(k) + "_" + u.String()
}

// pick returns a stable pseudo-random number in [0, n) for (salt, i).
func pick(salt string, i, n int) int {
	if n <= 0 {
		return 0
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("pando-load/pick/%s/%d", salt, i)))
	return int(binary.BigEndian.Uint64(sum[:8]) % uint64(n)) //nolint:gosec // G115: the result is below n, which is an int.
}

// Names. The prefixes are how the run, the report and a person reading the
// database tell seeded rows from the install's own.
const (
	userPrefix  = "load-u"
	groupPrefix = "load-g"
	appPrefix   = "load-a"
	realPrefix  = "load-real-"
	tokenPrefix = "load-token-"
)

func userName(i int) string  { return fmt.Sprintf("%s%06d", userPrefix, i) }
func groupName(g int) string { return fmt.Sprintf("%s%05d", groupPrefix, g) }
func appSlug(j int) string   { return fmt.Sprintf("%s%05d", appPrefix, j) }
func realName(k int) string  { return fmt.Sprintf("%s%03d", realPrefix, k) }
func tokenName(t int) string { return fmt.Sprintf("%s%05d", tokenPrefix, t) }

func userID(i int) string  { return seedID(id.User, "user", i) }
func groupID(g int) string { return seedID(id.Group, "group", g) }
func appID(j int) string   { return seedID(id.App, "app", j) }
func specID(j int) string  { return seedID(id.Spec, "spec", j) }
func tokenID(t int) string { return seedID(id.Token, "token", t) }

// appOwner is the index of the user who owns app j, scattered across all
// users. Some people own two or more apps, as they would.
func appOwner(t Tier, j int) int { return pick("owner", j, t.Users) }

// userGroups are the groups user i is in: one to three of them.
func userGroups(t Tier, i int) []int {
	n := 1 + pick("group-count", i, 3)
	seen := map[int]bool{}
	out := make([]int, 0, n)
	for k := 0; k < n; k++ {
		g := pick(fmt.Sprintf("group-%d", k), i, t.Groups)
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out
}

// Address modes for seeded apps: half are reached at their own hostname,
// half at a path. Port mode is left to the real apps, which need a listener.
func appUsesHostname(j int) bool { return j%2 == 0 }

func appHostname(j int, baseDomain string) string { return appSlug(j) + "." + baseDomain }
func appPath(j int) string                        { return "/load/" + appSlug(j) }

// grantRow is one row of the grants table.
type grantRow struct {
	ID            string
	AppID         *string
	Plane         string
	PrincipalKind string
	PrincipalID   *string
	RoleID        *string
	RoleScope     string
}

func strp(s string) *string { return &s }

// appGrants are app j's grants: the owner's two (as
// state.Apps.Create writes them, R-073), and a deterministic share of the
// other kinds a real install accumulates — a group's use (60% of apps),
// two named people's use (30%), anyone's use (10%, R-074), and a group
// that may look at the app in the console (10%).
func appGrants(t Tier, j int) []grantRow {
	app := appID(j)
	owner := userID(appOwner(t, j))
	rows := []grantRow{
		{ID: seedID(id.Grant, "owner-control", j), AppID: strp(app), Plane: "control", PrincipalKind: "user",
			PrincipalID: strp(owner), RoleID: strp("role_owner"), RoleScope: "app"},
		{ID: seedID(id.Grant, "owner-data", j), AppID: strp(app), Plane: "data", PrincipalKind: "user",
			PrincipalID: strp(owner), RoleScope: "app"},
	}
	if pick("grant-group", j, 10) < 6 {
		rows = append(rows, grantRow{ID: seedID(id.Grant, "group-data", j), AppID: strp(app), Plane: "data",
			PrincipalKind: "group", PrincipalID: strp(groupID(pick("grant-group-which", j, t.Groups))), RoleScope: "app"})
	}
	if pick("grant-users", j, 10) < 3 {
		for k := 0; k < 2; k++ {
			u := userID(pick(fmt.Sprintf("grant-user-%d", k), j, t.Users))
			if u == owner {
				continue // the owner already has one; the unique index would refuse a second
			}
			rows = append(rows, grantRow{ID: seedID(id.Grant, fmt.Sprintf("user-data-%d", k), j), AppID: strp(app),
				Plane: "data", PrincipalKind: "user", PrincipalID: strp(u), RoleScope: "app"})
		}
	}
	if pick("grant-anon", j, 10) < 1 {
		rows = append(rows, grantRow{ID: seedID(id.Grant, "anon-data", j), AppID: strp(app), Plane: "data",
			PrincipalKind: "anonymous", RoleScope: "app"})
	}
	if pick("grant-viewer", j, 10) < 1 {
		rows = append(rows, grantRow{ID: seedID(id.Grant, "group-control", j), AppID: strp(app), Plane: "control",
			PrincipalKind: "group", PrincipalID: strp(groupID(pick("grant-viewer-which", j, t.Groups))),
			RoleID: strp("role_viewer"), RoleScope: "app"})
	}
	return dedupeGrants(rows)
}

// dedupeGrants drops a second grant to the same principal on the same plane,
// which grants_unique_principal refuses: two picks can land on one user.
func dedupeGrants(rows []grantRow) []grantRow {
	seen := map[string]bool{}
	out := rows[:0]
	for _, r := range rows {
		key := r.Plane + "/" + r.PrincipalKind + "/"
		if r.PrincipalID != nil {
			key += *r.PrincipalID
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	return out
}

// adminGrant is user i's install-wide administrator grant (R-080, R-081).
func adminGrant(i int) grantRow {
	return grantRow{ID: seedID(id.Grant, "admin", i), Plane: "control", PrincipalKind: "user",
		PrincipalID: strp(userID(i)), RoleID: strp("role_administrator"), RoleScope: "install"}
}

// seedSource is where every seeded app says it comes from: a published image,
// as the replicas test's prebuilt spec does.
var seedSource = spec.Source{Type: "image", Image: "nginx:alpine"}

// seededSpecTemplate is test/replicas' prebuiltSpec, the smallest spec that
// deploys. Seeded apps are never deployed (they stay stopped), but the body
// is a real one, so anything that reads pinned specs reads the same shape it
// would in an install.
const seededSpecTemplate = `{
	"schema_version": 1,
	"source": {"type": "image", "image": "nginx:alpine"},
	"build": {"strategy": "prebuilt"},
	"workloads": [{"name": "web", "primary": true, "exposed": true,
		"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
	"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": 9000},
	"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
	"deploy": {"strategy": "recreate"}
}`

// appSpec is app j's pinned revision: the template, addressed by hostname or
// path.
func appSpec(t Tier, j int, baseDomain, createdBy string) (spec.AppSpec, error) {
	var s spec.AppSpec
	if err := json.Unmarshal([]byte(seededSpecTemplate), &s); err != nil {
		return spec.AppSpec{}, err
	}
	s.AppID = appID(j)
	s.Revision = 1
	s.CreatedAt = seedEpoch
	s.CreatedBy = createdBy
	s.Origin = spec.OriginManual
	if appUsesHostname(j) {
		s.Routing = spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingSubdomain, Hostname: appHostname(j, baseDomain)}
	} else {
		s.Routing = spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPath, PathPrefix: appPath(j)}
	}
	return s, nil
}

// realAppSpec is the request body for a real app's spec: the replicas test's
// prebuiltSpec. In path mode: a port written into a spec is kept as written and
// never allocated, so nothing would listen on it. Seeding moves the app to port
// mode through the routing endpoint, which allocates one.
func realAppSpec() string {
	return `{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "path"},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`
}

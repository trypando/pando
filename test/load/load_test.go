package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/id"
)

func TestSeedIDsAreWellFormedStableAndDistinct(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		for _, got := range []string{userID(i), groupID(i), appID(i), specID(i), tokenID(i)} {
			require.False(t, seen[got], "duplicate ID %s", got)
			seen[got] = true
		}
		require.True(t, id.Is(id.User, userID(i)))
		require.True(t, id.Is(id.Group, groupID(i)))
		require.True(t, id.Is(id.App, appID(i)))
		require.True(t, id.Is(id.Spec, specID(i)))
		require.True(t, id.Is(id.Token, tokenID(i)))
	}
	require.Equal(t, appID(7), appID(7), "a second seed computes the same ID")
	require.Less(t, appID(1), appID(2), "IDs sort in index order, as ULIDs sort by time")
}

func TestGrantsFitTheSchemasRules(t *testing.T) {
	tier := Tiers["vm"]
	seenIDs := map[string]bool{}
	kinds := map[string]int{}
	for j := 0; j < tier.Apps; j++ {
		unique := map[string]bool{}
		for _, g := range appGrants(tier, j) {
			require.False(t, seenIDs[g.ID], "grant IDs are unique")
			seenIDs[g.ID] = true
			require.True(t, id.Is(id.Grant, g.ID))

			// grants_unique_principal
			key := g.Plane + "/" + g.PrincipalKind
			if g.PrincipalID != nil {
				key += "/" + *g.PrincipalID
			}
			require.False(t, unique[key], "app %d has two %s grants", j, key)
			unique[key] = true

			// grants_anonymous_has_no_principal, grants_role_is_control_plane_only,
			// grants_install_has_no_app
			require.Equal(t, g.PrincipalKind == "anonymous", g.PrincipalID == nil)
			require.Equal(t, g.Plane == "control", g.RoleID != nil)
			require.Equal(t, "app", g.RoleScope)
			require.NotNil(t, g.AppID)
			kinds[g.Plane+"/"+g.PrincipalKind]++
		}
	}
	for _, k := range []string{"control/user", "data/user", "data/group", "data/anonymous", "control/group"} {
		require.Positive(t, kinds[k], "the vm tier seeds some %s grants", k)
	}
	require.Equal(t, tier.Apps, kinds["control/user"], "every app has its owner's control grant (R-073)")

	a := adminGrant(0)
	require.Equal(t, "install", a.RoleScope)
	require.Nil(t, a.AppID)
	require.Equal(t, "role_administrator", *a.RoleID)
}

func TestSeededSpecsValidateAndAddressesAreUsable(t *testing.T) {
	tier := Tiers["vm"]
	hosts, paths := map[string]bool{}, map[string]bool{}
	for j := 0; j < 50; j++ {
		s, err := appSpec(tier, j, "localtest.me", "usr_x")
		require.NoError(t, err)
		require.NoError(t, spec.Validate(&s), "app %d's spec", j)
		switch s.Routing.Mode {
		case spec.RoutingSubdomain:
			require.False(t, hosts[s.Routing.Hostname])
			hosts[s.Routing.Hostname] = true
		case spec.RoutingPath:
			require.Nil(t, spec.CheckPathPrefix(s.Routing.PathPrefix))
			require.False(t, paths[s.Routing.PathPrefix])
			paths[s.Routing.PathPrefix] = true
		default:
			t.Fatalf("app %d has mode %s", j, s.Routing.Mode)
		}
	}
	require.NotEmpty(t, hosts)
	require.NotEmpty(t, paths)
}

func TestUserGroupsAreOneToThreeDistinctGroups(t *testing.T) {
	tier := Tiers["vm"]
	for i := 0; i < 500; i++ {
		gs := userGroups(tier, i)
		require.GreaterOrEqual(t, len(gs), 1)
		require.LessOrEqual(t, len(gs), 3)
		seen := map[int]bool{}
		for _, g := range gs {
			require.False(t, seen[g])
			seen[g] = true
			require.Less(t, g, tier.Groups)
		}
	}
}

func TestRampScalesThePeak(t *testing.T) {
	steps := Ramp(Tiers["cluster"], DefaultFractions, time.Minute)
	require.Len(t, steps, 5)
	require.Equal(t, 500, steps[0].ConsoleUsers)
	require.Equal(t, 50.0, steps[0].APIRate)
	require.Equal(t, 200.0, steps[0].ProxyRate)
	require.Equal(t, 5000, steps[4].ConsoleUsers)
	require.Equal(t, 2000.0, steps[4].ProxyRate)
	for i, s := range steps {
		require.Equal(t, i, s.Index)
		require.Equal(t, time.Minute, s.Hold)
	}

	small := Ramp(Tier{ConsoleUsers: 3}, []float64{0.1}, time.Minute)
	require.Equal(t, 1, small[0].ConsoleUsers, "a step always has someone online")
}

func TestParseFractions(t *testing.T) {
	got, err := parseFractions("")
	require.NoError(t, err)
	require.Equal(t, DefaultFractions, got)
	got, err = parseFractions("0.5, 1,2")
	require.NoError(t, err)
	require.Equal(t, []float64{0.5, 1, 2}, got)
	_, err = parseFractions("half")
	require.Error(t, err)
	_, err = parseFractions("0")
	require.Error(t, err)
}

func TestTierValidate(t *testing.T) {
	for name, tier := range Tiers {
		require.NoError(t, tier.Validate(), name)
	}
	bad := Tiers["vm"]
	bad.Tokens = bad.Apps + 1
	require.Error(t, bad.Validate())
	_, err := TierByName("huge")
	require.Error(t, err)
}

func TestPacerReleasesAtTheRateWithoutDrift(t *testing.T) {
	p := &Pacer{Rate: 100}
	total := 0
	for ms := 10; ms <= 10_000; ms += 10 {
		total += p.Due(time.Duration(ms) * time.Millisecond)
	}
	require.Equal(t, 1000, total, "ten seconds at 100/s is 1000, whatever the tick")

	// A late tick catches up rather than losing what it missed.
	p = &Pacer{Rate: 2.5}
	require.Equal(t, 0, p.Due(300*time.Millisecond))
	require.Equal(t, 5, p.Due(2*time.Second))
	require.Equal(t, 0, p.Due(2100*time.Millisecond))
	require.Equal(t, 0, (&Pacer{Rate: 0}).Due(time.Hour))
}

func TestHistogramQuantilesAreWithinTwoPercent(t *testing.T) {
	h := NewHistogram()
	for i := 1; i <= 1000; i++ {
		h.Add(time.Duration(i) * time.Millisecond)
	}
	for _, c := range []struct {
		q    float64
		want time.Duration
	}{{0.5, 500 * time.Millisecond}, {0.95, 950 * time.Millisecond}, {0.99, 990 * time.Millisecond}} {
		got := h.Quantile(c.q)
		require.InEpsilon(t, float64(c.want), float64(got), 0.021, "p%.0f", c.q*100)
	}
	require.Equal(t, time.Second, h.Max)
	require.Equal(t, time.Second, h.Quantile(1), "the top quantile is the exact maximum")

	o := NewHistogram()
	o.Add(5 * time.Second)
	h.Merge(o)
	require.Equal(t, uint64(1001), h.Total)
	require.Equal(t, 5*time.Second, h.Max)

	require.Zero(t, NewHistogram().Quantile(0.5))
	big := NewHistogram()
	big.Add(time.Hour)
	require.Equal(t, time.Hour, big.Quantile(0.5), "past the last bucket, the maximum is still exact")
}

func TestRecorderKeepsOnlyTheHoldExceptSignIns(t *testing.T) {
	r := NewRecorder()
	r.Record(Outcome{Surface: "console", Class: "GET /me", Status: 200, OK: true, Latency: time.Millisecond})
	r.Record(Outcome{Surface: "sign-in", Class: "POST /sessions", Status: 200, OK: true, Latency: time.Second, Always: true})
	r.Hold()
	r.Record(Outcome{Surface: "console", Class: "GET /me", Status: 200, OK: true, Latency: time.Millisecond})
	r.Record(Outcome{Surface: "console", Class: "GET /me", Status: 500, OK: false, Latency: time.Millisecond})
	r.Record(Outcome{Surface: "console", Class: "GET /me", Err: errors.New("reset"), Latency: time.Millisecond})
	got := r.Take(10 * time.Second)
	require.Len(t, got, 2)
	me := got[0]
	require.Equal(t, "GET /me", me.Class)
	require.Equal(t, uint64(3), me.Requests)
	require.Equal(t, uint64(2), me.Errors)
	require.Equal(t, map[string]uint64{"200": 1, "500": 1, "transport error": 1}, me.Statuses)
	require.InDelta(t, 0.3, me.Rate(), 1e-9)
	require.Equal(t, "POST /sessions", got[1].Class)

	r.Record(Outcome{Surface: "console", Class: "GET /me", Status: 200, OK: true})
	require.Empty(t, r.Take(time.Second), "Take stops recording")
}

func class(surface, name string, requests, errors uint64, latency time.Duration) *ClassStats {
	h := NewHistogram()
	for range requests {
		h.Add(latency)
	}
	return &ClassStats{Class: name, Surface: surface, Requests: requests, Errors: errors,
		Statuses: map[string]uint64{"200": requests - errors, "500": errors}, Latency: h, Seconds: 10}
}

func TestBreachesAreJudgedAgainstTheFirstStep(t *testing.T) {
	th := DefaultThresholds
	first := StepResult{Classes: []*ClassStats{
		class("console", "GET /apps/{id}/usage", 100, 100, 10*time.Millisecond), // refuses at every load
		class("console", "GET /me", 100, 0, 10*time.Millisecond),
	}}
	require.Empty(t, Breaches(nil, first, th), "an endpoint that always fails is not load breaking it")

	later := StepResult{
		Classes: []*ClassStats{
			class("console", "GET /apps/{id}/usage", 100, 100, 10*time.Millisecond),
			class("console", "GET /me", 100, 5, 10*time.Millisecond),
			class("proxy", "seeded app by path", 100, 0, 2*time.Second),
			class("api", "GET /apps", 5, 5, 10*time.Second), // too few to judge
			class("sign-in", "POST /sessions", 100, 0, 3*time.Second),
		},
		PG: PGSample{MaxConnections: 100, PeakTotal: 95},
	}
	got := Breaches([]StepResult{first}, later, th)
	var classes []string
	for _, b := range got {
		classes = append(classes, b.Class)
	}
	require.ElementsMatch(t, []string{"GET /me", "seeded app by path", "postgres"}, classes,
		"slow sign-ins are argon2id doing its job, not a breach")
}

func TestReportSaysWhereItBroke(t *testing.T) {
	steps := Ramp(Tiers["vm"], []float64{0.5, 1}, time.Minute)
	held := StepResult{Step: steps[0], Online: 150, Classes: []*ClassStats{
		class("console", "GET /apps/{id}/status", 1000, 0, 40*time.Millisecond),
	}, PG: PGSample{MaxConnections: 400, PeakTotal: 70, Samples: 12},
		TopQueries: []Query{{Query: "SELECT a.id\n  FROM apps a | x", Calls: 10, TotalMS: 1234, MeanMS: 123.4, Rows: 10}}}
	broke := StepResult{Step: steps[1], Online: 300, Classes: []*ClassStats{
		class("console", "GET /apps/{id}/status", 1000, 0, 4*time.Second),
	}, Dropped: map[string]uint64{"proxy": 12}, PG: PGSample{MaxConnections: 400, PeakTotal: 90}}
	broke.Breaches = Breaches([]StepResult{held}, broke, DefaultThresholds)
	require.NotEmpty(t, broke.Breaches)

	var b strings.Builder
	require.NoError(t, WriteReport(&b, []Results{{
		Tier: Tiers["vm"], Replicas: 2, Commit: "abc1234", Statements: true,
		Thresholds: DefaultThresholds, Steps: []StepResult{held, broke},
		Seeded: map[string]int{"users": 3001, "apps": 1010},
	}}))
	out := b.String()
	require.Contains(t, out, "## Tier: vm")
	require.Contains(t, out, "**Broke at step 2** (300 console users, 50 API req/s, 200 proxy req/s); the last step that held was step 1")
	require.Contains(t, out, "GET /apps/{id}/status: p95 4.0 s (limit 1.0 s)")
	require.Contains(t, out, "**GET /apps/{id}/status**")
	require.Contains(t, out, "dropped: proxy 12")
	require.Contains(t, out, "| 1.2 s | 10 | 123.40 ms | 10 | `SELECT a.id FROM apps a \\| x` |", "queries fit one table cell")
	require.Contains(t, out, "apps 1010, users 3001")
	require.Contains(t, out, "did not finish")

	b.Reset()
	require.NoError(t, WriteReport(&b, []Results{{Tier: Tiers["vm"], Thresholds: DefaultThresholds, Steps: []StepResult{held}}}))
	require.Contains(t, b.String(), "**Held** through step 1")

	held.Classes = append(held.Classes, class("console", "GET /apps/{id}/usage", 100, 100, time.Millisecond))
	b.Reset()
	require.NoError(t, WriteReport(&b, []Results{{Tier: Tiers["vm"], Thresholds: DefaultThresholds, Steps: []StepResult{held}}}))
	require.Contains(t, b.String(), "**Held** through step 1")
	require.Contains(t, b.String(), "Failing from the first step")
	require.Contains(t, b.String(), "- console GET /apps/{id}/usage: 100.00% errors (200×0, 500×100)")
}

func TestMillisecondFormatting(t *testing.T) {
	require.Equal(t, "0 ms", ms(0))
	require.Equal(t, "2.5 ms", ms(2500*time.Microsecond))
	require.Equal(t, "250 ms", ms(250*time.Millisecond))
	require.Equal(t, "1.2 s", ms(1234*time.Millisecond))
	require.Equal(t, "12.0 s", ms(12*time.Second))
}

func TestPersonas(t *testing.T) {
	require.Equal(t, PersonaAdmin, personaOf(wsUser{Admin: true, Owns: []string{"app_x"}}))
	require.Equal(t, PersonaOwner, personaOf(wsUser{Owns: []string{"app_x"}}))
	require.Equal(t, PersonaLauncher, personaOf(wsUser{}))

	users := []wsUser{{ID: "a"}, {ID: "b", Admin: true}, {ID: "c"}, {ID: "d"}, {ID: "e", Admin: true}}
	order := onlineOrder(users)
	require.Len(t, order, 5)
	require.ElementsMatch(t, []int{1, 4}, order[:2], "administrators come online first")
	require.Equal(t, order, onlineOrder(users), "the order is the same every run")
}

func TestRedactURL(t *testing.T) {
	require.Equal(t, "postgres://pando:[redacted]@localhost:25432/pando?sslmode=disable",
		redactURL("postgres://pando:hunter2@localhost:25432/pando?sslmode=disable"))
	require.Equal(t, "postgres://localhost/pando", redactURL("postgres://localhost/pando"))
}

package assist

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
)

// bigService is a Service over an installation of n accounts, apps and
// groups, every one of which matches the letter "a", with fake stores that
// fail any unbounded read.
func bigService(t *testing.T, ai *fakeAI, n int) *Service {
	t.Helper()
	s := newService(t, ai, "")
	users := make([]state.User, n)
	apps := make([]state.App, n)
	groups := make([]state.Group, n)
	for i := range n {
		users[i] = state.User{ID: fmt.Sprintf("usr_%05d", i), ExternalID: fmt.Sprintf("alice%05d", i),
			DisplayName: fmt.Sprintf("Alice Anders %d", i), Email: fmt.Sprintf("alice%05d@example.com", i)}
		apps[i] = state.App{ID: fmt.Sprintf("app_%05d", i), Name: fmt.Sprintf("analytics-%d", i), Slug: fmt.Sprintf("analytics-%d", i)}
		groups[i] = state.Group{ID: fmt.Sprintf("grp_%05d", i), Name: fmt.Sprintf("Analysts %d", i)}
	}
	s.Users = fakeUsers{users: users}
	s.Apps = fakeApps{apps: apps}
	s.Groups = fakeGroups{groups: groups}
	return s
}

func size(t *testing.T, v ...any) int {
	t.Helper()
	body, err := json.Marshal(v)
	require.NoError(t, err)
	return len(body)
}

// TestO54_NoOperationReadsAWholeTable asserts O-54: on an installation of
// ten thousand accounts, apps and groups, drafting access and searching the
// audit log succeed with stores that refuse any unbounded read, and what is
// sent to the adapter stays small, whether the adapter looks things up or
// core pre-searches for it.
func TestO54_NoOperationReadsAWholeTable(t *testing.T) {
	const n = 10_000
	for _, looks := range []bool{false, true} {
		t.Run(fmt.Sprintf("looks_up=%t", looks), func(t *testing.T) {
			caps := doesAll()
			caps.LooksUp = looks
			ai := &fakeAI{caps: caps, lookFor: []string{"", "a", "alice", "analytics", "analysts"},
				access: api.AccessDraft{Group: &api.GroupDraft{Name: "Analysts new", Members: []string{"usr_00001", "usr_00002"}}},
				search: api.AuditSearch{Filter: api.AuditFilter{PrincipalID: "alice00001", Actions: []string{"app."}}},
			}
			s := bigService(t, ai, n)
			s.Audit = &fakeAudit{records: records(250)}
			ctx := context.Background()

			current := &api.AccessDraft{Group: &api.GroupDraft{Name: "x", Members: make([]string, 0, n)}}
			for i := range n {
				current.Group.Members = append(current.Group.Members, fmt.Sprintf("usr_%05d", i))
			}
			res, err := s.DraftAccess(ctx, admin, "Alice Anders and every analyst should be able to deploy analytics apps", current)
			require.NoError(t, err)
			require.NoError(t, ai.lookErr)
			require.NotNil(t, res.Group)
			assert.Equal(t, []string{"usr_00001", "usr_00002"}, res.Group.Members)

			req := ai.accessReq
			assert.LessOrEqual(t, len(req.People), 2*api.LookupLimit)
			assert.LessOrEqual(t, len(req.Groups), api.LookupLimit)
			assert.Less(t, size(t, req.People, req.Groups), 16<<10, "the lists sent do not grow with the install")
			assert.Equal(t, looks, req.Lookup != nil)
			assert.LessOrEqual(t, len(ai.looked.people), len(ai.lookFor)*api.LookupLimit)
			assert.LessOrEqual(t, len(ai.looked.groups), len(ai.lookFor)*api.LookupLimit)

			_, err = s.SearchAudit(ctx, admin, "what did alice00001 do to the analytics apps last week")
			require.NoError(t, err)
			require.NoError(t, ai.lookErr)
			assert.Equal(t, "usr_00001", ai.summaryReq.Filter.PrincipalID)
			assert.Less(t, size(t, ai.searchReq.People, ai.searchReq.Apps), 16<<10)
			assert.Less(t, size(t, ai.summaryReq.People, ai.summaryReq.Apps), 16<<10)
			assert.Equal(t, looks, ai.searchReq.Lookup != nil)
			assert.LessOrEqual(t, len(ai.looked.apps), 2*len(ai.lookFor)*api.LookupLimit)
		})
	}
}

// TestO54_TheFakeStoresRefuseAnUnboundedRead asserts the stores the test
// above runs against do refuse what it says they refuse.
func TestO54_TheFakeStoresRefuseAnUnboundedRead(t *testing.T) {
	ctx := context.Background()
	_, err := fakeUsers{}.Search(ctx, "", 0)
	require.ErrorIs(t, err, errUnbounded)
	_, _, _, err = fakeUsers{}.ListPage(ctx, state.Page{Limit: -1})
	require.ErrorIs(t, err, errUnbounded)
	_, _, _, err = fakeApps{}.ListAllPage(ctx, state.Page{Limit: state.MaxPageSize})
	require.ErrorIs(t, err, errUnbounded)
	_, err = fakeGroups{}.Search(ctx, "", 100)
	require.ErrorIs(t, err, errUnbounded)
}

// TestO54_LookupsReturnOnlyWhatThePersonMaySee asserts the lookups run as
// the person who asked: accounts and groups only with install.view, every
// app only with install.apps.view, and otherwise only the apps they hold a
// grant on — what GET /users, GET /groups and GET /apps would show them.
func TestO54_LookupsReturnOnlyWhatThePersonMaySee(t *testing.T) {
	caps := doesAll()
	caps.LooksUp = true
	ctx := context.Background()

	newOne := func(verbs []string) (*Service, *fakeAI, *int, *int) {
		ai := &fakeAI{caps: caps, lookFor: []string{"a", "ada", "blog", "ops"}}
		s := newService(t, ai, "")
		every, mine := 0, 0
		s.Apps = fakeApps{apps: []state.App{{ID: "app_blog", Name: "blog"}, {ID: "app_admin", Name: "admin-panel"}},
			granted: map[string]bool{"app_blog": true}, every: &every, mine: &mine}
		s.Verbs = fakeVerbs{verbs: verbs}
		return s, ai, &every, &mine
	}

	// An administrator sees every account, group and app.
	s, ai, every, mine := newOne(adminVerbs)
	_, err := s.DraftAccess(ctx, admin, "let ada into ops", nil)
	require.NoError(t, err)
	require.NotNil(t, ai.accessReq.Lookup.People)
	require.NotNil(t, ai.accessReq.Lookup.Groups)
	assert.NotEmpty(t, ai.looked.people)
	assert.NotEmpty(t, ai.looked.groups)
	assert.Len(t, ai.looked.apps, 2, "a finds admin-panel and blog finds blog")
	assert.Positive(t, *every)
	assert.Zero(t, *mine)

	// Someone who manages users but cannot view the install is offered no
	// account or group lookup, and sees only the apps granted to them.
	someone := authz.Principal{Kind: "user", ID: "usr_someone", UserID: "usr_someone"}
	s, ai, every, mine = newOne([]string{"install.users.manage"})
	_, err = s.DraftAccess(ctx, someone, "let ada into ops", nil)
	require.NoError(t, err)
	assert.Nil(t, ai.accessReq.Lookup.People)
	assert.Nil(t, ai.accessReq.Lookup.Groups)
	assert.Empty(t, ai.looked.people)
	for _, a := range ai.looked.apps {
		assert.Equal(t, "app_blog", a.ID, "only an app they hold a grant on")
	}
	assert.Zero(t, *every)
	assert.Positive(t, *mine)

	// Audit search offers no group lookup, whoever asks.
	s, ai, _, _ = newOne(adminVerbs)
	_, err = s.SearchAudit(ctx, admin, "what did ada do")
	require.NoError(t, err)
	assert.NotNil(t, ai.searchReq.Lookup.People)
	assert.Nil(t, ai.searchReq.Lookup.Groups)

	// The same holds for core's own search for an adapter that cannot look
	// things up, and for the names a summary is given.
	s, ai, _, _ = newOne([]string{"install.audit.read"})
	ai.caps.LooksUp = false
	s.Audit = &fakeAudit{records: records(2)}
	_, err = s.SearchAudit(ctx, someone, "what did ada do to the admin-panel")
	require.NoError(t, err)
	assert.Empty(t, ai.searchReq.People, "accounts are not theirs to read")
	assert.Empty(t, ai.searchReq.Apps, "admin-panel is not granted to them")
	assert.Empty(t, ai.summaryReq.People)
	assert.Equal(t, []api.AppInfo{{ID: "app_blog", Name: "blog"}}, ai.summaryReq.Apps)
}

// TestO54_AnAdapterThatCannotLookUpGetsMatchesForTheRequestsWords asserts
// O-54's [P] fallback: the words of the request are searched, at most
// LookupLimit matches of each kind are sent, and the adapter gets no Lookup.
func TestO54_AnAdapterThatCannotLookUpGetsMatchesForTheRequestsWords(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	s.Groups = fakeGroups{groups: []state.Group{{ID: "grp_ops", Name: "Ops"}, {ID: "grp_dev", Name: "Developers"}}}

	_, err := s.DraftAccess(context.Background(), admin, "Put bob@example.com into Ops for the blog", nil)
	require.NoError(t, err)
	assert.Nil(t, ai.accessReq.Lookup)
	require.Len(t, ai.accessReq.People, 1)
	assert.Equal(t, "usr_bob", ai.accessReq.People[0].ID)
	assert.Equal(t, []api.GroupInfo{{ID: "grp_ops", Name: "Ops"}}, ai.accessReq.Groups)

	ai.caps.LooksUp = true
	_, err = s.DraftAccess(context.Background(), admin, "Put bob@example.com into Ops for the blog", nil)
	require.NoError(t, err)
	assert.NotNil(t, ai.accessReq.Lookup)
	assert.Empty(t, ai.accessReq.People, "an adapter that looks things up is sent no pre-searched list")
	assert.Empty(t, ai.accessReq.Groups)
}

// TestO54_ARefinementNamesThePeopleInTheDraftSoFar asserts the members of
// the draft being refined are named for the model, by ID, at most
// LookupLimit of them.
func TestO54_ARefinementNamesThePeopleInTheDraftSoFar(t *testing.T) {
	caps := doesAll()
	caps.LooksUp = true
	ai := &fakeAI{caps: caps}
	s := newService(t, ai, "")
	current := &api.AccessDraft{Group: &api.GroupDraft{Name: "Release", Members: []string{"usr_bob", "usr_ghost"}}}

	_, err := s.DraftAccess(context.Background(), admin, "add Ada too", current)
	require.NoError(t, err)
	require.Len(t, ai.accessReq.People, 1)
	assert.Equal(t, "usr_bob", ai.accessReq.People[0].ID)
}

// TestO54_AGroupDraftOfMoreThanAPageIsCut asserts a drafted group naming
// more people than one bounded read checks keeps the first page and says so.
func TestO54_AGroupDraftOfMoreThanAPageIsCut(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := bigService(t, ai, state.MaxPageSize+10)
	members := make([]string, 0, state.MaxPageSize+10)
	for i := range state.MaxPageSize + 10 {
		members = append(members, fmt.Sprintf("usr_%05d", i))
	}
	ai.access = api.AccessDraft{Group: &api.GroupDraft{Name: "Everyone else", Members: members}}

	res, err := s.DraftAccess(context.Background(), admin, "a group of everyone", nil)
	require.NoError(t, err)
	require.NotNil(t, res.Group)
	assert.Len(t, res.Group.Members, state.MaxPageSize)
	require.Len(t, res.Refused, 1)
	assert.Contains(t, res.Refused[0], "a draft holds at most 500")
}

func TestTermsAreTheNamingWordsOfARequest(t *testing.T) {
	assert.Equal(t, []string{"ada@example.com", "lovelace", "release", "ops"},
		terms("Give Ada@Example.com, Lovelace and the release ops access to all apps!"))
	assert.Equal(t, []string{"o'brien"}, terms("what did O'Brien do?"))
	assert.Empty(t, terms("who can do it"))

	many := terms("one1 two2 three four five6 seven eight nine ten eleven twelve")
	assert.Len(t, many, maxTerms)
	assert.Equal(t, "eleven", many[0], "longest first")
}

func TestLookupErrorsStopTheRequest(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(s *Service){
		"people": func(s *Service) { s.Users = fakeUsers{err: errBoom} },
		"apps":   func(s *Service) { s.Apps = fakeApps{err: errBoom} },
		"groups": func(s *Service) { s.Groups = fakeGroups{err: errBoom} },
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			ai := &fakeAI{caps: doesAll()}
			s := newService(t, ai, "")
			breakIt(s)
			_, err := s.DraftAccess(ctx, admin, "let ada deploy", nil)
			require.ErrorIs(t, err, errBoom)
		})
	}

	// A refinement whose members cannot be read.
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	s.Users = fakeUsers{err: errBoom}
	_, err := s.DraftAccess(ctx, admin, "x", &api.AccessDraft{Group: &api.GroupDraft{Name: "g", Members: []string{"usr_ada"}}})
	require.ErrorIs(t, err, errBoom)

	// A summary whose names cannot be read.
	for name, breakIt := range map[string]func(s *Service){
		"people": func(s *Service) { s.Users = fakeUsers{err: errBoom} },
		"apps":   func(s *Service) { s.Apps = fakeApps{err: errBoom} },
	} {
		t.Run("summary "+name, func(t *testing.T) {
			caps := doesAll()
			caps.LooksUp = true
			ai := &fakeAI{caps: caps}
			s := newService(t, ai, "")
			s.Audit = &fakeAudit{records: records(1)}
			breakIt(s)
			_, err := s.SearchAudit(ctx, admin, "x")
			require.ErrorIs(t, err, errBoom)
			assert.False(t, ai.summaryRan)
		})
	}

	// A drafted group whose name or members cannot be checked.
	ai = &fakeAI{caps: doesAll(), access: api.AccessDraft{Group: &api.GroupDraft{Name: "New", Members: []string{"usr_ada"}}}}
	ai.caps.LooksUp = true
	s = newService(t, ai, "")
	s.Groups = fakeGroups{err: errBoom}
	_, err = s.DraftAccess(ctx, admin, "x", nil)
	require.ErrorIs(t, err, errBoom)

	s = newService(t, ai, "")
	s.Users = fakeUsers{err: errBoom}
	_, err = s.DraftAccess(ctx, admin, "x", nil)
	require.ErrorIs(t, err, errBoom)

	// A filter naming someone who cannot be looked up.
	for _, f := range []api.AuditFilter{{PrincipalID: "ada"}, {Involving: "ada"}} {
		ai = &fakeAI{caps: doesAll(), search: api.AuditSearch{Filter: f}}
		ai.caps.LooksUp = true
		s = newService(t, ai, "")
		s.Users = fakeUsers{err: errBoom}
		a := &fakeAudit{}
		s.Audit = a
		_, err = s.SearchAudit(ctx, admin, "x")
		require.ErrorIs(t, err, errBoom)
		assert.False(t, a.listed)
	}

	// Reading what the person holds.
	s = newService(t, &fakeAI{caps: doesAll()}, "")
	s.Verbs = fakeVerbs{err: errBoom}
	_, err = s.SearchAudit(ctx, admin, "x")
	require.ErrorIs(t, err, errBoom)
}

// TestO54_AuditSearchOffersTheCodesActionCatalog asserts the action names an
// audit search is given are audit.Actions, whatever the log holds.
func TestO54_AuditSearchOffersTheCodesActionCatalog(t *testing.T) {
	ai := &fakeAI{caps: doesAll()}
	s := newService(t, ai, "")
	_, err := s.SearchAudit(context.Background(), admin, "who created apps")
	require.NoError(t, err)
	assert.Equal(t, audit.Actions, ai.searchReq.Actions)
}

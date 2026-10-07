package assist

import (
	"context"
	"sort"
	"strings"
	"unicode"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
)

// Finding people, apps and groups for the AI (O-54).
//
// The adapter is never handed a whole table. One that LooksUp gets an
// api.Lookup whose functions search here, as the person who asked; any other
// gets what searchMatches finds for the words of the request. Either way each
// search is bounded by api.LookupLimit, and every result is something the
// person could read through the API: accounts and groups with install.view
// (GET /users, GET /groups), apps by the rule GET /apps applies.

// Bounds on the fallback search, which runs without a model choosing what to
// look up.
const (
	// maxTerms is the most words of a request searched for.
	maxTerms = 8

	// perTerm is the most matches of one kind kept for one word, so a common
	// word cannot crowd out the rest.
	perTerm = 5

	// groupNameProbe is how many groups are read to find one with exactly a
	// drafted group's name.
	groupNameProbe = 50
)

// sight is what one person may see, read once per request.
type sight struct {
	p    authz.Principal
	held map[string]bool
}

func (s *Service) sightOf(ctx context.Context, p authz.Principal) (sight, error) {
	v := sight{p: p, held: map[string]bool{}}
	if s.Verbs == nil {
		return v, nil
	}
	verbs, err := s.Verbs.InstallVerbsFor(ctx, p)
	if err != nil {
		return sight{}, err
	}
	for _, verb := range verbs {
		v.held[verb] = true
	}
	return v, nil
}

// people is whether the person may read accounts and groups: install.view,
// the verb GET /users and GET /groups require.
func (v sight) people() bool { return v.held[string(authz.InstallView)] }

// everyApp is whether the person sees every app (install.apps.view), as
// GET /apps decides; otherwise they see the apps they hold a grant on.
func (v sight) everyApp() bool { return v.held[string(authz.InstallAppsView)] }

func (s *Service) findPeople(ctx context.Context, v sight, q string, limit int) ([]api.PersonInfo, error) {
	if !v.people() {
		return nil, nil
	}
	users, err := s.Users.Search(ctx, strings.TrimSpace(q), limit)
	if err != nil {
		return nil, err
	}
	return personInfos(users), nil
}

func (s *Service) findApps(ctx context.Context, v sight, page state.Page) ([]api.AppInfo, error) {
	var (
		apps []state.App
		err  error
	)
	if v.everyApp() {
		apps, _, _, err = s.Apps.ListAllPage(ctx, page)
	} else {
		apps, _, _, err = s.Apps.ListForPrincipalPage(ctx, v.p, page)
	}
	if err != nil {
		return nil, err
	}
	out := make([]api.AppInfo, 0, len(apps))
	for _, a := range apps {
		out = append(out, api.AppInfo{ID: a.ID, Name: a.Name, Slug: a.Slug})
	}
	return out, nil
}

func (s *Service) findGroups(ctx context.Context, v sight, q string, limit int) ([]api.GroupInfo, error) {
	if !v.people() {
		return nil, nil
	}
	groups, err := s.Groups.Search(ctx, strings.TrimSpace(q), limit)
	if err != nil {
		return nil, err
	}
	out := make([]api.GroupInfo, 0, len(groups))
	for _, g := range groups {
		out = append(out, api.GroupInfo{ID: g.ID, Name: g.Name})
	}
	return out, nil
}

// peopleByID are the accounts among the first limit of ids that the person
// may see.
func (s *Service) peopleByID(ctx context.Context, v sight, ids []string, limit int) ([]api.PersonInfo, error) {
	ids = firstUnique(ids, limit)
	if !v.people() || len(ids) == 0 {
		return nil, nil
	}
	users, _, _, err := s.Users.ListPage(ctx, state.Page{IDs: ids, Limit: len(ids)})
	if err != nil {
		return nil, err
	}
	return personInfos(users), nil
}

// appsByID are the apps among the first limit of ids that the person may
// see.
func (s *Service) appsByID(ctx context.Context, v sight, ids []string, limit int) ([]api.AppInfo, error) {
	ids = firstUnique(ids, limit)
	if len(ids) == 0 {
		return nil, nil
	}
	return s.findApps(ctx, v, state.Page{IDs: ids, Limit: len(ids)})
}

// lookup is the api.Lookup for an adapter that LooksUp: each function runs as
// the person, and one they may not use is nil, so its tool is not offered.
func (s *Service) lookup(v sight, groups bool) *api.Lookup {
	l := &api.Lookup{
		Apps: func(ctx context.Context, q string) ([]api.AppInfo, error) {
			return s.findApps(ctx, v, state.Page{Limit: api.LookupLimit, Query: strings.TrimSpace(q)})
		},
	}
	if v.people() {
		l.People = func(ctx context.Context, q string) ([]api.PersonInfo, error) {
			return s.findPeople(ctx, v, q, api.LookupLimit)
		}
		if groups {
			l.Groups = func(ctx context.Context, q string) ([]api.GroupInfo, error) {
				return s.findGroups(ctx, v, q, api.LookupLimit)
			}
		}
	}
	return l
}

// found is what searchMatches found.
type found struct {
	people []api.PersonInfo
	apps   []api.AppInfo
	groups []api.GroupInfo
}

// searchMatches is O-54's fallback for an adapter that cannot look things up.
// [P] Each of the request's first maxTerms words is searched for as a
// person, an app and (when groups is set) a group, keeping perTerm matches
// of each kind per word and api.LookupLimit of each kind in all. A name the
// request does not spell out is not found, and the model is told the lists
// are matches, not everything.
func (s *Service) searchMatches(ctx context.Context, v sight, text string, groups bool) (found, error) {
	var out found
	seen := map[string]bool{}
	for _, term := range terms(text) {
		people, err := s.findPeople(ctx, v, term, perTerm)
		if err != nil {
			return found{}, err
		}
		for _, p := range people {
			if !seen[p.ID] && len(out.people) < api.LookupLimit {
				seen[p.ID] = true
				out.people = append(out.people, p)
			}
		}
		apps, err := s.findApps(ctx, v, state.Page{Limit: perTerm, Query: term})
		if err != nil {
			return found{}, err
		}
		for _, a := range apps {
			if !seen[a.ID] && len(out.apps) < api.LookupLimit {
				seen[a.ID] = true
				out.apps = append(out.apps, a)
			}
		}
		if !groups {
			continue
		}
		gs, err := s.findGroups(ctx, v, term, perTerm)
		if err != nil {
			return found{}, err
		}
		for _, g := range gs {
			if !seen[g.ID] && len(out.groups) < api.LookupLimit {
				seen[g.ID] = true
				out.groups = append(out.groups, g)
			}
		}
	}
	return out, nil
}

// stopWords are words a request is made of that name nobody and nothing.
var stopWords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "who": true, "can": true, "should": true,
	"able": true, "all": true, "any": true, "every": true, "give": true, "make": true, "let": true,
	"from": true, "into": true, "app": true, "apps": true, "group": true, "groups": true, "role": true,
	"roles": true, "access": true, "people": true, "person": true, "last": true, "month": true,
	"week": true, "day": true, "days": true, "year": true, "today": true, "yesterday": true, "did": true,
	"what": true, "which": true, "when": true, "where": true, "has": true, "have": true, "been": true,
	"this": true, "that": true, "them": true, "they": true, "their": true, "our": true, "out": true,
	"are": true, "was": true, "were": true, "not": true, "but": true, "who's": true, "anyone": true,
	"everyone": true, "someone": true, "called": true, "named": true, "only": true, "also": true,
	"new": true, "how": true, "many": true, "times": true, "since": true, "until": true, "between": true,
}

// terms are the words of text worth searching for: emails and names, not
// the words around them. At most maxTerms, each once, longest first so a
// full email or a surname is searched before a short word.
func terms(text string) []string {
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("@._-'", r)
	})
	seen := map[string]bool{}
	var out []string
	for _, w := range words {
		w = strings.Trim(strings.ToLower(w), ".'-_")
		if len([]rune(w)) < 3 || stopWords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out[:min(len(out), maxTerms)]
}

func personInfos(users []state.User) []api.PersonInfo {
	out := make([]api.PersonInfo, 0, len(users))
	for _, u := range users {
		out = append(out, api.PersonInfo{ID: u.ID, Username: u.ExternalID, Name: u.DisplayName, Email: u.Email})
	}
	return out
}

// firstUnique is the first n distinct non-empty values of ids.
func firstUnique(ids []string, n int) []string {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
		if len(out) == n {
			break
		}
	}
	return out
}

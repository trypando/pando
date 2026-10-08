package aikit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/trypando/pando/internal/adapter/api"
)

// The lookup tools (O-54): how a model drafting access or searching the audit
// log finds the people, apps and groups a request names, instead of being
// handed every one. Each runs a callback core passed in the request, as the
// person who asked; nothing here reads state (R-027).
const (
	ToolFindPeople = "find_people"
	ToolFindApps   = "find_apps"
	ToolFindGroups = "find_groups"

	// MaxLookups is the most lookups one conversation may make. Past it a
	// lookup is answered with a request to submit, not run, so a model that
	// keeps searching ends the conversation rather than growing it.
	MaxLookups = 10

	// maxLookupField is the most of one name, email or ID a result carries,
	// in characters, so a result is bounded by LookupLimit times this.
	maxLookupField = 200
)

// LookupTools are the tools l offers: one per lookup the person may make.
func LookupTools(l *api.Lookup) []Tool {
	if l == nil {
		return nil
	}
	query := map[string]any{
		"query": map[string]any{"type": "string", "description": "Part of a name, username, email or slug. Case does not matter."},
	}
	var out []Tool
	if l.People != nil {
		out = append(out, Tool{Name: ToolFindPeople, Properties: query, Required: []string{"query"},
			Description: fmt.Sprintf("Find accounts by username, display name or email. Returns at most %d, "+
				"each with its account ID. Search once per person named; use a shorter part of the name when "+
				"nothing matches.", api.LookupLimit)})
	}
	if l.Apps != nil {
		out = append(out, Tool{Name: ToolFindApps, Properties: query, Required: []string{"query"},
			Description: fmt.Sprintf("Find apps by name or slug. Returns at most %d, each with its app ID.", api.LookupLimit)})
	}
	if l.Groups != nil {
		out = append(out, Tool{Name: ToolFindGroups, Properties: query, Required: []string{"query"},
			Description: fmt.Sprintf("Find existing groups by name. Returns at most %d, each with its group ID.", api.LookupLimit)})
	}
	return out
}

// LookupNote is what a prompt says about the lookup tools, when it has them.
const LookupNote = "The lists below are not every account, app or group: look up anyone or anything the " +
	"request names with the find tools, and use the IDs they return. Never make up an ID. If a lookup " +
	"finds nobody, leave them out and say so."

// MatchNote is what a prompt says about lists core pre-searched, for an
// adapter without the lookup tools.
const MatchNote = "The lists below are not every account, app or group: they are the ones whose names " +
	"match words in the request. If someone or something the request names is not listed, leave it out " +
	"and say so; never make up an ID."

// Looker runs the lookup tools for one conversation, counting them.
type Looker struct {
	l     *api.Lookup
	calls int
}

// NewLooker returns a Looker over l, which may be nil.
func NewLooker(l *api.Lookup) *Looker { return &Looker{l: l} }

// Handles reports whether name is a lookup tool this conversation offers.
func (k *Looker) Handles(name string) bool {
	if k == nil || k.l == nil {
		return false
	}
	switch name {
	case ToolFindPeople:
		return k.l.People != nil
	case ToolFindApps:
		return k.l.Apps != nil
	case ToolFindGroups:
		return k.l.Groups != nil
	}
	return false
}

// Calls is how many lookups have run.
func (k *Looker) Calls() int { return k.calls }

// Call runs one lookup and says what to hand back: the text, and whether it
// is an error. The result is at most LookupLimit matches, each field clipped.
func (k *Looker) Call(ctx context.Context, name, input string) (string, bool) {
	if !k.Handles(name) {
		return "There is no tool by that name.", true
	}
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(input), &in); err != nil {
		return "That request could not be read: " + err.Error(), true
	}
	if k.calls >= MaxLookups {
		return fmt.Sprintf("You have made all %d lookups this request allows. Submit your answer now, "+
			"with what you found.", MaxLookups), false
	}
	k.calls++
	query := clip(strings.TrimSpace(in.Query))

	var (
		found any
		n     int
		err   error
	)
	switch name {
	case ToolFindPeople:
		var people []api.PersonInfo
		people, err = k.l.People(ctx, query)
		people = people[:min(len(people), api.LookupLimit)]
		for i := range people {
			p := &people[i]
			p.ID, p.Username, p.Name, p.Email = clip(p.ID), clip(p.Username), clip(p.Name), clip(p.Email)
		}
		found, n = people, len(people)
	case ToolFindApps:
		var apps []api.AppInfo
		apps, err = k.l.Apps(ctx, query)
		apps = apps[:min(len(apps), api.LookupLimit)]
		for i := range apps {
			apps[i].ID, apps[i].Name = clip(apps[i].ID), clip(apps[i].Name)
		}
		found, n = apps, len(apps)
	case ToolFindGroups:
		var groups []api.GroupInfo
		groups, err = k.l.Groups(ctx, query)
		groups = groups[:min(len(groups), api.LookupLimit)]
		for i := range groups {
			groups[i].ID, groups[i].Name = clip(groups[i].ID), clip(groups[i].Name)
		}
		found, n = groups, len(groups)
	}
	if err != nil {
		return "The lookup did not work: " + err.Error(), true
	}
	if n == 0 {
		return fmt.Sprintf("Nothing matches %q. Try a shorter part of the name, or leave it out and say so.", query), false
	}
	body, _ := json.Marshal(found) //nolint:errcheck // slices of plain structs always marshal.
	return string(body), false
}

// clip cuts s to maxLookupField characters.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxLookupField {
		return s
	}
	return string([]rune(s)[:maxLookupField])
}

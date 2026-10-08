package aikit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

func everyLookup(n int) *api.Lookup {
	return &api.Lookup{
		People: func(_ context.Context, q string) ([]api.PersonInfo, error) {
			out := make([]api.PersonInfo, n)
			for i := range out {
				out[i] = api.PersonInfo{ID: fmt.Sprintf("usr_%d", i), Username: q, Name: strings.Repeat("n", 1000)}
			}
			return out, nil
		},
		Apps: func(context.Context, string) ([]api.AppInfo, error) {
			out := make([]api.AppInfo, n)
			for i := range out {
				out[i] = api.AppInfo{ID: fmt.Sprintf("app_%d", i), Name: strings.Repeat("a", 1000)}
			}
			return out, nil
		},
		Groups: func(context.Context, string) ([]api.GroupInfo, error) {
			out := make([]api.GroupInfo, n)
			for i := range out {
				out[i] = api.GroupInfo{ID: fmt.Sprintf("grp_%d", i), Name: strings.Repeat("g", 1000)}
			}
			return out, nil
		},
	}
}

// TestO54_LookupToolsAreWhatTheRequestCarries asserts a tool is offered per
// lookup the request carries, and none without a Lookup.
func TestO54_LookupToolsAreWhatTheRequestCarries(t *testing.T) {
	assert.Empty(t, aikit.LookupTools(nil))
	names := func(tools []aikit.Tool) []string {
		var out []string
		for _, tool := range tools {
			out = append(out, tool.Name)
		}
		return out
	}
	assert.Equal(t, []string{"find_people", "find_apps", "find_groups"}, names(aikit.LookupTools(everyLookup(1))))
	assert.Equal(t, []string{"find_apps"}, names(aikit.LookupTools(&api.Lookup{Apps: everyLookup(1).Apps})))

	task := aikit.AccessTask(api.AccessRequest{Description: "x", Lookup: &api.Lookup{People: everyLookup(1).People}})
	assert.Equal(t, []string{"submit", "find_people"}, names(task.Tools()))
	assert.Contains(t, task.System, aikit.LookupNote)

	task = aikit.AuditSearchTask(api.AuditSearchRequest{Question: "x"})
	assert.Equal(t, []string{"submit"}, names(task.Tools()))
	assert.Contains(t, task.System, aikit.MatchNote)
}

// TestO54_ALookupResultIsBounded asserts a lookup hands back at most
// LookupLimit matches, each field clipped, however much the callback returns.
func TestO54_ALookupResultIsBounded(t *testing.T) {
	k := aikit.NewLooker(everyLookup(10_000))
	ctx := context.Background()
	for _, name := range []string{aikit.ToolFindPeople, aikit.ToolFindApps, aikit.ToolFindGroups} {
		text, isErr := k.Call(ctx, name, `{"query":"`+strings.Repeat("q", 1000)+`"}`)
		require.False(t, isErr, text)
		var got []map[string]any
		require.NoError(t, json.Unmarshal([]byte(text), &got))
		assert.Len(t, got, api.LookupLimit, name)
		assert.Less(t, len(text), api.LookupLimit*1000, "%s: names are clipped", name)
	}
}

// TestO54_LookupsPerConversationAreBounded asserts a conversation makes at
// most MaxLookups lookups; past that it is told to submit, and the callback
// is not run.
func TestO54_LookupsPerConversationAreBounded(t *testing.T) {
	calls := 0
	k := aikit.NewLooker(&api.Lookup{People: func(context.Context, string) ([]api.PersonInfo, error) {
		calls++
		return nil, nil
	}})
	for range aikit.MaxLookups {
		text, isErr := k.Call(context.Background(), aikit.ToolFindPeople, `{"query":"nobody"}`)
		require.False(t, isErr)
		assert.Contains(t, text, `Nothing matches "nobody"`)
	}
	text, isErr := k.Call(context.Background(), aikit.ToolFindPeople, `{"query":"one more"}`)
	assert.False(t, isErr, "a refusal the model can act on, not a failure")
	assert.Contains(t, text, "Submit your answer now")
	assert.Equal(t, aikit.MaxLookups, calls)
	assert.Equal(t, aikit.MaxLookups, k.Calls())
}

func TestLookerRefusesWhatItCannotRun(t *testing.T) {
	ctx := context.Background()
	var nothing *aikit.Looker
	assert.False(t, nothing.Handles(aikit.ToolFindPeople))
	assert.False(t, aikit.NewLooker(nil).Handles(aikit.ToolFindPeople))

	k := aikit.NewLooker(&api.Lookup{Apps: func(context.Context, string) ([]api.AppInfo, error) {
		return nil, errors.New("the database is away")
	}})
	assert.False(t, k.Handles(aikit.ToolFindPeople), "not offered, so not run")
	assert.False(t, k.Handles(aikit.ToolFindGroups))
	assert.False(t, k.Handles("read_file"))

	text, isErr := k.Call(ctx, aikit.ToolFindPeople, `{"query":"x"}`)
	assert.True(t, isErr)
	assert.Equal(t, "There is no tool by that name.", text)

	text, isErr = k.Call(ctx, aikit.ToolFindApps, `not json`)
	assert.True(t, isErr)
	assert.Contains(t, text, "could not be read")

	text, isErr = k.Call(ctx, aikit.ToolFindApps, `{"query":"blog"}`)
	assert.True(t, isErr)
	assert.Equal(t, "The lookup did not work: the database is away", text)
	assert.Equal(t, 1, k.Calls())
}

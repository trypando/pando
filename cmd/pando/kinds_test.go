package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	oidcidentity "github.com/trypando/pando/internal/adapter/identity/oidc"
	samlidentity "github.com/trypando/pando/internal/adapter/identity/saml"
	"github.com/trypando/pando/internal/reference"
)

// TestR261_EveryKindCanBeSetUpFromItsBasicSettings asserts R-261 for the
// forms every surface builds from GET /adapters/kinds: each kind this build
// registers describes its settings so that one filled in with only its basic
// settings works — an advanced setting is never required, never a credential,
// and states the default it takes when left empty (issue #85).
func TestR261_EveryKindCanBeSetUpFromItsBasicSettings(t *testing.T) {
	kinds := append(adapterKinds(), oidcidentity.Info(), samlidentity.Info())
	for _, k := range kinds {
		require.NoError(t, k.Validate())
	}
}

// TestR261_EveryKindSaysHowItIsConfigured asserts R-261 for the adapter
// reference (docs/adapters.json) and the forms built from the same data: every
// kind this build registers, identity providers included, has a name and a
// description, and every setting says what it is for — in its help text, or,
// for a choice, in a description of each option. An adapter cannot ship with a
// setting only its author can fill in.
func TestR261_EveryKindSaysHowItIsConfigured(t *testing.T) {
	kinds := allKinds()
	require.NotEmpty(t, kinds)
	for _, k := range kinds {
		where := string(k.Category) + "/" + k.Kind
		require.NotEmpty(t, k.Name, "%s has no name", where)
		require.NotEmpty(t, k.Description, "%s has no description", where)
		for _, f := range k.Fields {
			require.NotEmpty(t, f.Label, "%s setting %s has no label", where, f.Key)
			if f.Help != "" {
				continue
			}
			require.NotEmpty(t, f.Options, "%s setting %s has no help: say what it is for", where, f.Key)
			for _, o := range f.Options {
				require.NotEmpty(t, o.Description,
					"%s setting %s has no help, and its option %q no description: say what choosing it means", where, f.Key, o.Value)
			}
		}
	}

	_, err := reference.Adapters(kinds)
	require.NoError(t, err, "every kind's category is in api.Categories, so the reference lists it")
}

// TestR261_KindValidationRefusesAnAdvancedSettingThatCannotBeLeftEmpty
// asserts R-261: Validate refuses each way an advanced setting could
// make the basic form fail.
func TestR261_KindValidationRefusesAnAdvancedSettingThatCannotBeLeftEmpty(t *testing.T) {
	kind := func(f adapterapi.Field) adapterapi.KindInfo {
		return adapterapi.KindInfo{Category: "ai", Kind: "test", Fields: []adapterapi.Field{f}}
	}
	for name, f := range map[string]adapterapi.Field{
		"required":   {Key: "a", Type: "string", Required: true, Default: "x", Advanced: true},
		"credential": {Key: "a", Type: "string", Credential: true, Default: "x", Advanced: true},
		"no default": {Key: "a", Type: "string", Advanced: true},
	} {
		require.Error(t, kind(f).Validate(), name)
	}
	require.NoError(t, kind(adapterapi.Field{Key: "a", Type: "bool", Advanced: true}).Validate(),
		"a bool left alone is off, which is its default")
	require.NoError(t, kind(adapterapi.Field{Key: "a", Type: "int", Default: "30", Advanced: true}).Validate())
}

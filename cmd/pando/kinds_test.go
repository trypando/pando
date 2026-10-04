package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	oidcidentity "github.com/trypando/pando/internal/adapter/identity/oidc"
	samlidentity "github.com/trypando/pando/internal/adapter/identity/saml"
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

// TestR261_KindValidationRefusesAnAdvancedSettingThatCannotBeLeftEmpty
// asserts R-261: the check above refuses each way an advanced setting could
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

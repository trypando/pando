package api

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func stepsKind() KindInfo {
	return KindInfo{Category: CategoryRouting, Kind: "example",
		Fields: []Field{
			{Key: "method", Label: "Method", Type: "select", Default: "token"},
			{Key: "token", Label: "Token", Type: "string", Credential: true, ShownWhen: &Condition{Key: "method", Values: []string{"token"}}},
			{Key: "zone", Label: "Zone", Type: "string", Required: true},
			{Key: "image", Label: "Image", Type: "string", Default: "x", Advanced: true},
		},
		Steps: []Step{
			{ID: "method", Label: "Method", Title: "Choose a method", Fields: []string{"method"}},
			{ID: "token", Label: "Token", Title: "Create a token", Fields: []string{"token"},
				ShownWhen: &Condition{Key: "method", Values: []string{"token"}},
				Links:     []Link{{Label: "Open tokens", URL: "https://{zone}/tokens"}}},
			{ID: "zone", Label: "Zone", Title: "Choose a zone", Fields: []string{"zone"}},
		}}
}

// Guided setup asks every basic setting once, in an order that works, and the
// form can rely on each step being complete (design 03 §1.2).
func TestStepsAreCheckedForEveryWayThrough(t *testing.T) {
	require.NoError(t, stepsKind().Validate())

	cases := map[string]func(k *KindInfo){
		"no ID":            func(k *KindInfo) { k.Steps[0].ID = "" },
		"two steps named":  func(k *KindInfo) { k.Steps[1].ID = "method" },
		"no label":         func(k *KindInfo) { k.Steps[0].Label = "" },
		"no title":         func(k *KindInfo) { k.Steps[0].Title = "" },
		"asks for no":      func(k *KindInfo) { k.Steps[2].Fields = nil },
		"not one of its":   func(k *KindInfo) { k.Steps[2].Fields = []string{"zone", "nope"} },
		"advanced setting": func(k *KindInfo) { k.Steps[2].Fields = []string{"zone", "image"} },
		"on both step":     func(k *KindInfo) { k.Steps[2].Fields = []string{"zone", "method"} },
		"is on no step":    func(k *KindInfo) { k.Steps = k.Steps[:2] },
		"no earlier step":  func(k *KindInfo) { k.Steps[0], k.Steps[1] = k.Steps[1], k.Steps[0] },
		"label and an":     func(k *KindInfo) { k.Steps[1].Links[0].URL = "" },
		"links with {nope}": func(k *KindInfo) {
			k.Steps[1].Links[0].URL = "https://{nope}/"
		},
		"empty instruction": func(k *KindInfo) { k.Steps[1].Do = []Action{{}} },
		"which is not asked before it": func(k *KindInfo) {
			k.Steps[0].Fields, k.Steps[1].ShownWhen = []string{"token", "method"}, nil
		},
		"not one of its presets": func(k *KindInfo) { k.Steps[2].Presets = []string{"acme"} },
		"suggests from":          func(k *KindInfo) { k.Fields[2].Suggest = "pando.{nope}" },
		"is a list":              func(k *KindInfo) { k.Fields[1].List = true },
	}
	for want, change := range cases {
		k := stepsKind()
		change(&k)
		require.ErrorContains(t, k.Validate(), want, want)
	}
}

// A kind with presets is checked once per provider a person can pick: each
// sees its own steps, and a named one may leave out what its preset settles.
func TestStepsAreCheckedForEachProvider(t *testing.T) {
	k := KindInfo{Category: CategoryAuditSink, Kind: "example",
		Fields: []Field{
			{Key: "url", Label: "URL", Type: "string"},
			{Key: "token", Label: "Token", Type: "string", Credential: true},
			{Key: "body", Label: "Body", Type: "select", Default: "ndjson"},
		},
		Presets:     []Preset{{ID: "splunk", Label: "Splunk", Values: map[string]string{"body": "hec"}}},
		OtherPreset: "Another SIEM",
		Steps: []Step{
			{ID: "splunk_token", Label: "Token", Title: "Create an HEC token", Fields: []string{"token"}, Presets: []string{"splunk"}},
			{ID: "token", Label: "Token", Title: "Enter the token", Fields: []string{"token", "body"}, Presets: []string{PresetOther}},
			{ID: "url", Label: "URL", Title: "Enter the endpoint", Fields: []string{"url"}},
		}}
	require.NoError(t, k.Validate())

	// Any provider may leave out what is not required, but never what is.
	k.Steps[1].Fields = []string{"token"}
	require.NoError(t, k.Validate())
	k.Fields[2].Required = true
	require.ErrorContains(t, k.Validate(), `for provider "other" setting "body" is on no step`)
	k.Fields[2].Required = false

	// A step for the other choice needs a kind that offers one.
	k.Steps[1].Fields = []string{"token", "body"}
	k.OtherPreset = ""
	require.ErrorContains(t, k.Validate(), `provider "other", which is not one of its presets`)
}

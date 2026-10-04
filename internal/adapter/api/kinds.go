package api

import "fmt"

// KindInfo describes a kind of adapter this build of Pando can run: what it is
// called, what it is for, and the settings configuring one takes.
//
// Data, like capabilities (R-254). Each adapter package describes itself, and
// the console and the CLI build their "add an adapter" forms from it — so a
// kind added to Pando is configurable from every surface without either being
// taught about it (R-261).
type KindInfo struct {
	Category    Category `json:"category"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Fields      []Field  `json:"fields"`

	// IDPrefix is the prefix an instance's ID conventionally takes, such as
	// "ai_" — so a form can suggest "ai_anthropic" rather than ask for one.
	IDPrefix string `json:"id_prefix"`

	// Presets are known providers of this kind — Okta, Microsoft Entra ID —
	// with the settings each needs filled in, so connecting one is choosing it
	// rather than knowing its quirks. A form offers them; the settings are the
	// same ones Fields describes.
	Presets []Preset `json:"presets,omitempty"`
}

// Preset is a starting configuration for a known provider.
type Preset struct {
	ID    string `json:"id"`
	Label string `json:"label"`

	// Help says where in the provider's own console to find what the form
	// still asks for.
	Help string `json:"help,omitempty"`

	// Values are settings to fill in, keyed like Fields.
	Values map[string]string `json:"values,omitempty"`
}

// Field is one setting.
type Field struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Help  string `json:"help,omitempty"`

	// Type is "string", "int", "bool" or "select": how a form should ask for
	// it and how the value goes into the configuration's JSON. A select's value
	// is a string.
	Type string `json:"type"`

	// Options are what a "select" offers.
	Options []Option `json:"options,omitempty"`

	// Other lets a "select" take a value not among Options, typed in — a DNS
	// provider Pando does not name, say.
	Other bool `json:"other,omitempty"`

	// Multiline asks for a text area: a credential given as KEY=value lines.
	Multiline bool `json:"multiline,omitempty"`

	// ShownWhen hides the setting unless another setting has one of the
	// given values — DNS provider fields only when certificates use DNS. A
	// form that ignores it still works; it only shows more than it needs to.
	ShownWhen *Condition `json:"shown_when,omitempty"`

	Required bool `json:"required,omitempty"`

	// Advanced marks a setting most people never change: a form asks for it
	// under "Advanced settings" rather than beside the ones that matter, and
	// the CLI lists it under its own heading. It means less common, not
	// restricted — anyone who may configure the adapter may set it. An advanced
	// setting is never required, never a credential, and states its Default,
	// so leaving every one empty sets the adapter up (Validate).
	Advanced bool `json:"advanced,omitempty"`

	// Credential marks a secret such as an API key. It is sent in the create
	// request's write-only "credentials", stored encrypted, and never shown
	// again (R-190) — never in "config", which the database refuses it in.
	Credential bool `json:"credential,omitempty"`

	// Default is the value used when the setting is left empty — the
	// adapter's own default, stated so a form can show it rather than an
	// example that might be mistaken for it.
	Default string `json:"default,omitempty"`

	// Placeholder is an example value, for a setting with no default.
	Placeholder string `json:"placeholder,omitempty"`
}

// Option is one choice a "select" offers.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`

	// Description says what choosing it means, for a short list shown as
	// radio buttons where the tradeoff should be visible.
	Description string `json:"description,omitempty"`
}

// Condition is a setting's value a form checks before showing another.
type Condition struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// Validate checks what a kind says about its settings that a form relies on.
//
// An advanced setting is one a form puts out of the way, so it must be safe to
// leave empty: not required, and with its default stated, so someone who never
// opens "Advanced settings" can still see what they get. Not a credential
// either: the CLI asks for secrets as it adds an adapter, and asks only for the
// basic settings.
func (k KindInfo) Validate() error {
	seen := map[string]bool{}
	for _, f := range k.Fields {
		if seen[f.Key] {
			return fmt.Errorf("%s/%s has two settings named %q", k.Category, k.Kind, f.Key)
		}
		seen[f.Key] = true
		if !f.Advanced {
			continue
		}
		switch {
		case f.Required:
			return fmt.Errorf("%s/%s setting %q is both required and advanced: a required setting belongs among the basic ones", k.Category, k.Kind, f.Key)
		case f.Credential:
			return fmt.Errorf("%s/%s setting %q is a credential marked advanced: the CLI asks for credentials only among the basic settings", k.Category, k.Kind, f.Key)
		case f.Default == "" && f.Type != "bool":
			return fmt.Errorf("%s/%s setting %q is advanced and states no default: say what leaving it empty does", k.Category, k.Kind, f.Key)
		}
	}
	return nil
}

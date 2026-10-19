package api

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/trypando/pando/internal/secret"
)

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

	// Check refuses a configuration as it is saved, in the words Configure
	// would refuse it at startup — a kind that needs no connection to know.
	// It is given the settings, the credentials sent, and the names of those
	// already stored and not sent again. Nil when a kind is only checked at
	// startup.
	Check func(config json.RawMessage, credentials map[string]secret.Value, stored []string) error `json:"-"`

	// Managed marks a kind Pando sets up itself, from its install topology,
	// such as the BuildKit it runs builds on. A form does not offer to add
	// one or to edit the one there is; its settings, all advanced, are set
	// through the API or the CLI.
	Managed bool `json:"managed,omitempty"`
	// OtherPreset names the choice, beside the presets, of a provider none of
	// them is — "Another provider", "Another SIEM". Guided setup asks which
	// provider first when a kind has presets; empty means the presets cover
	// every case, as github.com and GitHub Enterprise Server do. A step for
	// that choice lists PresetOther in its Presets.
	OtherPreset string `json:"other_preset,omitempty"`

	// Steps walk someone through setting one up, one screen at a time: what to
	// do in the provider's own console, then the settings that produces. Every
	// basic setting is asked on exactly one step; advanced ones on none — a
	// form shows them on its review, closed (Validate). Choosing the kind,
	// naming the adapter and reviewing it are the form's own steps, the same
	// for every kind, and are not listed here.
	Steps []Step `json:"steps,omitempty"`

	// Warn says what in a configuration works but may not do what someone
	// wants — a console hostname Cloudflare's included certificate does not
	// cover — each in a sentence that says what to do about it. Never a
	// reason to refuse a configuration: that is the adapter's Configure.
	// Saving one returns its warnings. Nil when a kind has none to give.
	Warn func(config json.RawMessage) []string `json:"-"`
}

// Step is one screen of guided setup.
type Step struct {
	// ID names the step for a form's progress and its "change" links.
	ID string `json:"id"`

	// Label is the step's name in a form's list of steps, a word or two:
	// "API token", "Domain".
	Label string `json:"label"`

	// Title is an instruction or a question, in sentence case: "Create an
	// API token", "Which domain should apps use?".
	Title string `json:"title"`

	// Lede is one sentence on why the step is asked, shown under the title.
	Lede string `json:"lede,omitempty"`

	// Do is what to do outside Pando, in order, one action to an item, in the
	// provider's own words. **Bold** marks the name of something on the
	// provider's screen and `backticks` a command, path or value typed
	// exactly; nothing else is markup. {key} stands for a setting's value, or
	// for one of the Refs the form supplies, such as {callback_url}.
	Do []Action `json:"do,omitempty"`

	// Links open the provider's pages the step starts on: usually one, and
	// one per way when there is more than one way to do it, as there is to
	// make a Cloudflare token.
	Links []Link `json:"links,omitempty"`

	// Fields are the keys of the settings asked on this step, in order.
	Fields []string `json:"fields"`

	// ShownWhen skips the step unless a setting asked on an earlier step has
	// one of the given values — the GitHub App steps only for a GitHub App.
	ShownWhen *Condition `json:"shown_when,omitempty"`

	// Presets are the providers the step is for, by preset ID, and
	// PresetOther for the choice of one no preset names: Splunk's token
	// instructions for Splunk only. Empty is every provider.
	Presets []string `json:"presets,omitempty"`
}

// Refs a form supplies for a step's text and links, beside the settings: an
// identity provider's addresses, which the person registers with the
// provider before Pando has saved it.
const (
	RefCallbackURL = "callback_url" // where the provider sends the browser back: the OIDC redirect URI, the SAML ACS URL
	RefEntityID    = "entity_id"    // the SAML service provider's entity ID, also its metadata URL
)

// PresetOther stands, in a step's Presets, for a provider none of the kind's
// presets names.
const PresetOther = "other"

// Action is one thing to do in a step.
type Action struct {
	Text string `json:"text"`

	// Table is shown under the text, for something with columns, such as an
	// API token's permissions.
	Table *Table `json:"table,omitempty"`
}

// Table is a small grid of plain text.
type Table struct {
	Columns []string   `json:"columns"`
	Rows    [][]string `json:"rows"`
}

// Link is a page on the provider's site. Its URL may hold {key} for a
// setting's value, as Suggest does — https://{host}/settings/apps — so a link
// follows a self-managed host.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
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

	// List asks for any number of values of a "string" setting in one field:
	// each one typed is kept as it is entered, as a form sees fit — the
	// console's a chip. They are sent as one string, separated by commas, so
	// an adapter reads a list the same way whichever surface set it.
	List bool `json:"list,omitempty"`

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

	// Presets are the providers the setting is for, by preset ID — a Google
	// Workspace domain for Google only. Guided setup shows it to no one else.
	// Empty is every provider.
	Presets []string `json:"presets,omitempty"`

	// Suggest is the value a form starts a new adapter's setting at, with
	// {key} standing for another setting's value: "pando.{zone}". It follows
	// that setting until the setting is edited, and is then the person's own.
	// Unlike Default it is sent as if typed; the adapter never applies it.
	Suggest string `json:"suggest,omitempty"`
}

// Option is one choice a "select" offers.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`

	// Description says what choosing it means, for a short list shown as
	// radio buttons where the tradeoff should be visible.
	Description string `json:"description,omitempty"`

	// Recommended marks the choice to make unless there is a reason not to.
	// At most one option of a setting carries it.
	Recommended bool `json:"recommended,omitempty"`
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
		if err := k.validateField(f); err != nil {
			return err
		}
	}
	return k.validateSteps()
}

// validateField checks one setting on its own.
func (k KindInfo) validateField(f Field) error {
	for _, id := range f.Presets {
		if !slices.ContainsFunc(k.Presets, func(p Preset) bool { return p.ID == id }) {
			return fmt.Errorf("%s/%s setting %q is for provider %q, which is not one of its presets", k.Category, k.Kind, f.Key, id)
		}
	}
	if f.List && (f.Type != "string" || f.Credential || f.Multiline) {
		return fmt.Errorf("%s/%s setting %q is a list: a list is a plain string setting, neither a credential nor multiline", k.Category, k.Kind, f.Key)
	}
	for _, m := range suggestRef.FindAllStringSubmatch(f.Suggest, -1) {
		if !k.hasField(m[1]) || m[1] == f.Key {
			return fmt.Errorf("%s/%s setting %q suggests from {%s}, which is not another of its settings", k.Category, k.Kind, f.Key, m[1])
		}
	}
	if f.Advanced {
		return k.validateAdvanced(f)
	}
	return nil
}

// validateAdvanced checks that an advanced setting is safe to leave empty.
func (k KindInfo) validateAdvanced(f Field) error {
	switch {
	case f.Required:
		return fmt.Errorf("%s/%s setting %q is both required and advanced: a required setting belongs among the basic ones", k.Category, k.Kind, f.Key)
	case f.Credential:
		return fmt.Errorf("%s/%s setting %q is a credential marked advanced: the CLI asks for credentials only among the basic settings", k.Category, k.Kind, f.Key)
	case f.Default == "" && f.Type != "bool":
		return fmt.Errorf("%s/%s setting %q is advanced and states no default: say what leaving it empty does", k.Category, k.Kind, f.Key)
	}
	return nil
}

// suggestRef is a {key} in a Suggest.
var suggestRef = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

func (k KindInfo) hasField(key string) bool {
	for _, f := range k.Fields {
		if f.Key == key {
			return true
		}
	}
	return false
}

// validateSteps checks that guided setup asks for every basic setting once, in
// an order that works: a setting, or a step, shown only for another setting's
// value comes after the step that asks for that value. A kind with presets is
// checked once for each provider a person can choose, since each sees only
// the steps for it.
func (k KindInfo) validateSteps() error {
	if len(k.Steps) == 0 {
		return nil
	}
	where := fmt.Sprintf("%s/%s", k.Category, k.Kind)
	stepIDs := map[string]bool{}
	for i, st := range k.Steps {
		if err := k.validateStep(where, i, st, stepIDs); err != nil {
			return err
		}
		stepIDs[st.ID] = true
	}
	for _, path := range k.paths() {
		var steps []Step
		for _, st := range k.Steps {
			if st.forPreset(path) {
				steps = append(steps, st)
			}
		}
		if err := k.validatePath(where, path, steps); err != nil {
			return err
		}
	}
	return nil
}

// validateStep checks one step on its own; stepIDs holds the IDs of the steps
// before it.
func (k KindInfo) validateStep(where string, i int, st Step, stepIDs map[string]bool) error {
	switch {
	case st.ID == "":
		return fmt.Errorf("%s step %d has no ID", where, i+1)
	case stepIDs[st.ID]:
		return fmt.Errorf("%s has two steps named %q", where, st.ID)
	case st.Label == "":
		return fmt.Errorf("%s step %q has no label: name it in a word or two for the list of steps", where, st.ID)
	case st.Title == "":
		return fmt.Errorf("%s step %q has no title: say what to do, or ask a question", where, st.ID)
	case len(st.Fields) == 0:
		return fmt.Errorf("%s step %q asks for no settings: fold its instructions into the step that uses them", where, st.ID)
	}
	for _, l := range st.Links {
		if l.Label == "" || l.URL == "" {
			return fmt.Errorf("%s step %q has a link without both a label and an address", where, st.ID)
		}
		if ref, ok := k.unknownRef(l.URL); !ok {
			return fmt.Errorf("%s step %q links with {%s}, which is not one of its settings", where, st.ID, ref)
		}
	}
	for _, a := range st.Do {
		if a.Text == "" {
			return fmt.Errorf("%s step %q has an empty instruction", where, st.ID)
		}
		if ref, ok := k.unknownRef(a.Text); !ok {
			return fmt.Errorf("%s step %q says {%s}, which is not one of its settings", where, st.ID, ref)
		}
	}
	for _, id := range st.Presets {
		if !k.hasPreset(id) {
			return fmt.Errorf("%s step %q is for provider %q, which is not one of its presets", where, st.ID, id)
		}
	}
	return nil
}

// unknownRef returns the first {key} in s that is neither a setting nor a
// value Pando fills in, and false; or "" and true when every one is known.
func (k KindInfo) unknownRef(s string) (string, bool) {
	for _, m := range suggestRef.FindAllStringSubmatch(s, -1) {
		if !k.hasField(m[1]) && m[1] != RefCallbackURL && m[1] != RefEntityID {
			return m[1], false
		}
	}
	return "", true
}

// hasPreset reports whether a step may name the preset: one of the kind's, or
// "other" when the kind offers it.
func (k KindInfo) hasPreset(id string) bool {
	if slices.ContainsFunc(k.Presets, func(p Preset) bool { return p.ID == id }) {
		return true
	}
	return id == PresetOther && k.OtherPreset != ""
}

// paths lists the providers a person can pick, each seeing the steps for it;
// with no presets, there is one way through.
func (k KindInfo) paths() []string {
	if len(k.Presets) == 0 {
		return []string{""}
	}
	paths := make([]string, 0, len(k.Presets)+1)
	for _, p := range k.Presets {
		paths = append(paths, p.ID)
	}
	if k.OtherPreset != "" {
		paths = append(paths, PresetOther)
	}
	return paths
}

// forPreset reports whether a step is shown to someone who chose the preset;
// a step that names none is for everyone.
func (st Step) forPreset(id string) bool {
	return id == "" || len(st.Presets) == 0 || slices.Contains(st.Presets, id)
}

// validatePath checks one way through the steps.
func (k KindInfo) validatePath(where, path string, steps []Step) error {
	if path != "" {
		where += " for provider " + strconv.Quote(path)
	}
	fields := map[string]Field{}
	for _, f := range k.Fields {
		fields[f.Key] = f
	}
	askedOn := map[string]int{} // field key -> index of the step asking it
	for i, st := range steps {
		if c := st.ShownWhen; c != nil && !askedBefore(askedOn, c.Key, i) {
			return fmt.Errorf("%s step %q is shown for setting %q, which no earlier step asks for", where, st.ID, c.Key)
		}
		for _, key := range st.Fields {
			if err := checkStepField(where, fields, steps, askedOn, i, key); err != nil {
				return err
			}
			askedOn[key] = i
		}
	}
	return k.checkCoverage(where, path, askedOn)
}

// askedBefore reports whether a step before the i'th asks for the setting.
func askedBefore(askedOn map[string]int, key string, i int) bool {
	j, ok := askedOn[key]
	return ok && j < i
}

// checkStepField checks one setting the i'th step asks for, against the
// settings the steps before it asked.
func checkStepField(where string, fields map[string]Field, steps []Step, askedOn map[string]int, i int, key string) error {
	st := steps[i]
	f, ok := fields[key]
	switch {
	case !ok:
		return fmt.Errorf("%s step %q asks for %q, which is not one of its settings", where, st.ID, key)
	case f.Advanced:
		return fmt.Errorf("%s step %q asks for advanced setting %q: advanced settings are shown on the review, not on a step", where, st.ID, key)
	}
	if j, dup := askedOn[key]; dup {
		return fmt.Errorf("%s asks for setting %q on both step %q and step %q", where, key, steps[j].ID, st.ID)
	}
	if c := f.ShownWhen; c != nil {
		if j, ok := askedOn[c.Key]; !ok || j > i {
			return fmt.Errorf("%s setting %q is shown for setting %q, which is not asked before it", where, key, c.Key)
		}
	}
	return nil
}

// checkCoverage checks that one way through the steps asks every basic
// setting it must.
//
// A provider's steps may leave out a setting that is not required: its
// preset settles it, or it does not apply — a Google Workspace domain to
// anyone else. Review shows it with the advanced ones. A kind without
// presets asks every setting.
func (k KindInfo) checkCoverage(where, path string, askedOn map[string]int) error {
	var values map[string]string
	for _, p := range k.Presets {
		if p.ID == path {
			values = p.Values
		}
	}
	for _, f := range k.Fields {
		if _, ok := askedOn[f.Key]; ok || f.Advanced || !f.appliesTo(path) {
			continue
		}
		if _, given := values[f.Key]; path != "" && (!f.Required || given) {
			continue
		}
		return fmt.Errorf("%s setting %q is on no step: every basic setting is asked on exactly one", where, f.Key)
	}
	return nil
}

// appliesTo reports whether a setting is shown to someone who chose the
// preset; a setting that names none is for everyone.
func (f Field) appliesTo(path string) bool {
	return path == "" || len(f.Presets) == 0 || slices.Contains(f.Presets, path)
}

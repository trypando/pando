package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// interactive is whether the command reads from a person at a terminal.
func interactive(cmd *cobra.Command) bool {
	f, ok := cmd.InOrStdin().(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// ref is a {key} in a suggestion or a link.
var ref = regexp.MustCompile(`\{([a-z0-9_]+)\}`)

// marks are step text's two marks, **bold** and `code`; a terminal shows
// neither, only the words.
var marks = regexp.MustCompile("\\*\\*([^*]+)\\*\\*|`([^`]+)`")

func plain(text string) string {
	return marks.ReplaceAllString(text, "$1$2")
}

// effective is a setting as the adapter will read it: what was given, or its
// default.
func effective(info *kindInfo, values map[string]string, key string) string {
	if v := values[key]; v != "" {
		return v
	}
	for _, f := range info.Fields {
		if f.Key == key {
			return f.Default
		}
	}
	return ""
}

func shown(info *kindInfo, values map[string]string, c *condition) bool {
	if c == nil {
		return true
	}
	v := effective(info, values, c.Key)
	for _, want := range c.Values {
		if v == want {
			return true
		}
	}
	return false
}

// fill replaces each {key} with that setting's value, or reports one empty.
func fill(info *kindInfo, values map[string]string, template string) (string, bool) {
	ok := true
	out := ref.ReplaceAllStringFunc(template, func(m string) string {
		v := effective(info, values, m[1:len(m)-1])
		if v == "" {
			ok = false
		}
		return v
	})
	return out, ok
}

// presetOther is the provider choice no preset names, in a step's presets.
const presetOther = "other"

// applyPreset fills in a known provider's values, leaving any already given.
func applyPreset(info *kindInfo, id string, values map[string]string) error {
	if id == presetOther && info.OtherPreset != "" {
		return nil
	}
	for _, p := range info.Presets {
		if p.ID == id {
			for k, v := range p.Values {
				if _, given := values[k]; !given {
					values[k] = v
				}
			}
			return nil
		}
	}
	ids := make([]string, 0, len(info.Presets)+1)
	for _, p := range info.Presets {
		ids = append(ids, p.ID)
	}
	if info.OtherPreset != "" {
		ids = append(ids, presetOther)
	}
	return fmt.Errorf("%s/%s has no provider %q; its providers are %s", info.Category, info.Kind, id, strings.Join(ids, ", "))
}

// askProvider asks which provider, for a kind with presets, and fills in its
// values. Enter keeps the first.
func askProvider(cmd *cobra.Command, info *kindInfo, values map[string]string) (string, error) {
	type choice struct{ id, label string }
	var choices []choice
	for _, p := range info.Presets {
		choices = append(choices, choice{p.ID, p.Label})
	}
	if info.OtherPreset != "" {
		choices = append(choices, choice{presetOther, info.OtherPreset})
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "\nChoose a provider")
	for n, c := range choices {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %d) %s\n", n+1, c.label)
	}
	v, err := prompt(cmd, "Provider [1]: ")
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	n := 1
	if v != "" {
		if n, err = strconv.Atoi(v); err != nil || n < 1 || n > len(choices) {
			return "", fmt.Errorf("choose a provider by its number, 1 to %d", len(choices))
		}
	}
	id := choices[n-1].id
	return id, applyPreset(info, id, values)
}

// walkSteps asks for each setting of each step that applies and --set did not
// give, showing the step's instructions first. A kind with presets asks which
// provider first, unless --preset said, and shows that provider's steps.
// Answers go into values, and secrets into credentials; it returns the keys
// it asked for or ruled out, which nothing asks for again.
func walkSteps(cmd *cobra.Command, info *kindInfo, preset string, values, credentials map[string]string) (map[string]bool, error) {
	out := cmd.ErrOrStderr()
	if preset == "" && len(info.Presets) > 0 {
		var err error
		if preset, err = askProvider(cmd, info, values); err != nil {
			return nil, err
		}
	}
	steps := stepsFor(info, preset)
	fields := map[string]kindField{}
	for _, f := range info.Fields {
		fields[f.Key] = f
	}
	asked := map[string]bool{}
	for i, st := range steps {
		pending := pendingFields(info, fields, values, st)
		if len(pending) == 0 {
			continue
		}
		fmt.Fprintf(out, "\n%s (step %d of %d)\n", st.Title, i+1, len(steps))
		printInstructions(out, info, values, st)
		for _, f := range pending {
			asked[f.Key] = true
			if err := ask(cmd, info, values, credentials, f); err != nil {
				return nil, err
			}
		}
	}
	ruleOut(info, asked)
	return asked, nil
}

// ruleOut marks every field on any step as handled: one on a step the walk
// skipped, or hidden by a choice made in it, was ruled out and is not asked
// for afterwards either.
func ruleOut(info *kindInfo, asked map[string]bool) {
	for _, st := range info.Steps {
		for _, key := range st.Fields {
			asked[key] = true
		}
	}
}

// stepsFor is the steps someone who chose the preset sees; a step that names
// no preset is for everyone.
func stepsFor(info *kindInfo, preset string) []kindStep {
	var steps []kindStep
	for _, st := range info.Steps {
		if preset == "" || len(st.Presets) == 0 || slices.Contains(st.Presets, preset) {
			steps = append(steps, st)
		}
	}
	return steps
}

// pendingFields is the settings a step still has to ask for: those shown for
// what was answered so far that --set did not give. A step that is not shown
// asks for none.
func pendingFields(info *kindInfo, fields map[string]kindField, values map[string]string, st kindStep) []kindField {
	if !shown(info, values, st.ShownWhen) {
		return nil
	}
	var pending []kindField
	for _, key := range st.Fields {
		f, ok := fields[key]
		if _, given := values[key]; ok && !given && shown(info, values, f.ShownWhen) {
			pending = append(pending, f)
		}
	}
	return pending
}

func printInstructions(out io.Writer, info *kindInfo, values map[string]string, st kindStep) {
	for n, a := range st.Do {
		fmt.Fprintf(out, "  %d. %s\n", n+1, plain(a.Text))
		if a.Table != nil {
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "     "+strings.Join(a.Table.Columns, "\t"))
			for _, row := range a.Table.Rows {
				fmt.Fprintln(w, "     "+strings.Join(row, "\t"))
			}
			_ = w.Flush()
		}
	}
	for _, l := range st.Links {
		if url, ok := fill(info, values, l.URL); ok {
			fmt.Fprintf(out, "  %s: %s\n", l.Label, url)
		}
	}
}

// ask asks for one setting. Enter keeps what it shows in brackets: its
// suggestion, filled in from what was answered before it, or its default.
func ask(cmd *cobra.Command, info *kindInfo, values, credentials map[string]string, f kindField) error {
	if f.Credential {
		return askSecret(cmd, credentials, f)
	}
	start := f.Default
	if f.Suggest != "" {
		if s, ok := fill(info, values, f.Suggest); ok {
			start = s
		}
	}
	for n, o := range f.Options {
		fmt.Fprintf(cmd.ErrOrStderr(), "  %d) %s\n", n+1, o.Label)
	}
	label := f.Label
	if start != "" {
		label += " [" + start + "]"
	}
	v, err := prompt(cmd, label+": ")
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	v = chosenOption(f, v)
	if v == "" && f.Suggest != "" {
		v = start
	}
	if v != "" {
		values[f.Key] = v
	}
	return nil
}

// askSecret asks for a credential without echoing it.
func askSecret(cmd *cobra.Command, credentials map[string]string, f kindField) error {
	v, err := promptSecret(cmd, f.Label+": ")
	if err != nil {
		return err
	}
	if v != "" {
		credentials[f.Key] = v
	}
	return nil
}

// chosenOption reads an answer to a setting with options: a number picks that
// option's value, and anything else is taken as typed.
func chosenOption(f kindField, v string) string {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= len(f.Options) {
		return f.Options[n-1].Value
	}
	return v
}

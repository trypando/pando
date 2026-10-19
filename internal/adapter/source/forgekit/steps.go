package forgekit

import "github.com/trypando/pando/internal/adapter/api"

// Guided-setup steps every forge asks the same way (api.KindInfo.Steps). A
// forge's steps.go lists its steps in order, taking these for the ones that
// read the same everywhere and writing its own where the forge's screens
// differ.

// CallbackPath is where a forge sends the browser back from an OAuth
// sign-in, under Pando's address: the source callback in httpapi.
const CallbackPath = "/api/v1/sources/callback"

// KeyScanAnyHost is SSHKey.KeyScan for a forge with no host of its own.
const KeyScanAnyHost = "`ssh-keyscan` with the host's name, on a machine you trust,"

// WhenMethod shows a step only for the given sign-in methods.
func WhenMethod(methods ...string) *api.Condition {
	return &api.Condition{Key: "method", Values: methods}
}

// MethodStep is a forge's first step: how Pando signs in, and the host.
func MethodStep() api.Step {
	return api.Step{ID: "method", Label: "Sign-in method", Title: "Choose how Pando signs in",
		Fields: []string{"method", "host"}}
}

// ScopeStep is a forge's last step: what the connection is limited to, in the
// forge's own word for it, such as an owner or a group.
func ScopeStep(label, title string) api.Step {
	return api.Step{ID: "scope", Label: label, Title: title, Fields: []string{"scope"}}
}

// SetCallback is the action that enters Pando's OAuth callback in the forge's
// field of that name.
func SetCallback(field string) string {
	return "Set **" + field + "** to your Pando address plus `" + CallbackPath + "`."
}

// OAuthStep registers an OAuth application on the forge and asks for its
// client ID and secret.
func OAuthStep(label, title string, links []api.Link, do ...string) api.Step {
	return api.Step{ID: "oauth", Label: label, Title: title, ShownWhen: WhenMethod(MethodOAuth),
		Do: actions(do...), Links: links, Fields: []string{"client_id", FieldClientSecret}}
}

// SSHKey is a forge's step for adding an SSH key.
type SSHKey struct {
	Label, Title string

	// File is the key's file name: ssh-keygen writes File and File.pub.
	File string

	// Type is ssh-keygen's key type arguments. Empty is -t ed25519.
	Type string

	// Add is how to add File.pub on the forge, in its own words.
	Add []string

	// KeyScan, when set, is the ssh-keyscan command to run for the host keys,
	// as it reads after "Run ".
	KeyScan string
}

// SSHKeyStep makes a key, adds its public half on the forge, and asks for the
// private half and, with KeyScan, the host keys.
func SSHKeyStep(k SSHKey) api.Step {
	typ := k.Type
	if typ == "" {
		typ = "-t ed25519"
	}
	do := []string{"Run `ssh-keygen " + typ + " -f " + k.File + " -N \"\"`."}
	do = append(do, k.Add...)
	do = append(do, "Paste `"+k.File+"` below.")
	if k.KeyScan != "" {
		do = append(do, "Run "+k.KeyScan+" and paste its output below.")
	}
	return api.Step{ID: "ssh", Label: k.Label, Title: k.Title, ShownWhen: WhenMethod(MethodSSH),
		Do: actions(do...), Fields: []string{FieldSSHPrivateKey, FieldSSHPassphrase, "known_hosts"}}
}

func actions(texts ...string) []api.Action {
	out := make([]api.Action, len(texts))
	for i, t := range texts {
		out[i] = api.Action{Text: t}
	}
	return out
}

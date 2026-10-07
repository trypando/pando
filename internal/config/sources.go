package config

import (
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Where each setting came from, so the console can say "this is set in the
// startup config, here" rather than leave an operator hunting for why a value
// will not change (R-271).

// Source is where a value came from.
type Source struct {
	// Kind is "env", "file" or "default".
	Kind string `json:"kind"`
	// Name is the environment variable, or the config file's path.
	Name string `json:"name,omitempty"`
	// Key is the key inside the config file.
	Key string `json:"key,omitempty"`
}

// Setting is one startup setting, its effective value and where it came from.
type Setting struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source Source `json:"source"`

	// Env is the variable that sets it, whether or not it is set — so a
	// setting still at its default can say how to change it.
	Env string `json:"env"`
}

// PolicySetting is a host policy field set at startup (a `policy:` section in
// the config file, or PANDO_POLICY_<FIELD>). It overrides the stored policy and
// cannot be changed from the console, the API or the CLI while it is set.
// The value is as given — a string from the environment, a typed value from
// the file — and is checked against the policy document by the policy package.
type PolicySetting struct {
	Key    string `json:"key"`
	Value  any    `json:"value"`
	Source Source `json:"source"`
}

// PolicyEnvPrefix is how a host policy field is set from the environment:
// PANDO_POLICY_MIN_SECURITY_SCORE sets min_security_score.
const PolicyEnvPrefix = "PANDO_POLICY_"

// secret keys are never reported, whatever their source (R-194). The database
// URL carries a password; the admin password is one.
var secret = map[string]bool{
	"database.url":             true,
	"bootstrap.admin_password": true,
	"registry.password":        true,
}

// envName is the variable a key is read from: its explicit bind, or the name
// the replacer derives.
func envName(key string) string {
	if name, ok := boundEnv[key]; ok {
		return name
	}
	return "PANDO_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// set reports whether an environment variable is set to something. Empty
// counts as unset, as it does for viper: Compose passes `${X:-}` through as an
// empty string, which is nobody's setting.
func set(name string) bool {
	v, ok := os.LookupEnv(name)
	return ok && v != ""
}

func sourceOf(v *viper.Viper, key, path string) Source {
	if name := envName(key); set(name) {
		return Source{Kind: "env", Name: name}
	}
	if path != "" && v.InConfig(key) {
		return Source{Kind: "file", Name: path, Key: key}
	}
	return Source{Kind: "default"}
}

// settingsOf lists every non-secret setting, sorted by key.
func settingsOf(v *viper.Viper, path string) []Setting {
	var out []Setting
	for _, key := range v.AllKeys() {
		// Policy and adapters are reported on their own, with what they fix.
		if secret[key] || strings.HasPrefix(key, "policy.") || key == "policy" ||
			strings.HasPrefix(key, "adapters.") || key == "adapters" {
			continue
		}
		value := v.Get(key)
		if d, ok := value.(time.Duration); ok {
			value = d.String()
		}
		out = append(out, Setting{Key: key, Value: value, Source: sourceOf(v, key, path), Env: envName(key)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// policyOf collects the host policy fields set at startup. The environment
// wins over the file, as it does for every other setting.
func policyOf(v *viper.Viper, path string) []PolicySetting {
	byKey := map[string]PolicySetting{}
	if path != "" {
		if section, ok := v.Get("policy").(map[string]any); ok {
			for key, value := range section {
				byKey[key] = PolicySetting{
					Key: key, Value: value,
					Source: Source{Kind: "file", Name: path, Key: "policy." + key},
				}
			}
		}
	}
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(name, PolicyEnvPrefix) || value == "" {
			continue
		}
		key := strings.ToLower(strings.TrimPrefix(name, PolicyEnvPrefix))
		byKey[key] = PolicySetting{Key: key, Value: value, Source: Source{Kind: "env", Name: name}}
	}
	out := make([]PolicySetting, 0, len(byKey))
	for _, s := range byKey {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

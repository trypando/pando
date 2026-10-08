package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/viper"

	"github.com/trypando/pando/internal/adapter/api"
)

// Adapters declared in the config file (R-271), for an install kept as code.
//
//	adapters:
//	  ai_anthropic:
//	    category: ai
//	    kind: anthropic
//	    name: Anthropic
//	    config:
//	      model: claude-opus-5-5
//	    credentials:
//	      api_key: {env: ANTHROPIC_API_KEY}
//	    functions:
//	      repair_plan: {}
//	      search_audit: {model: claude-haiku-4-5}
//
// A declared adapter is read-only everywhere else while it is declared: the
// API refuses to change it and says where it was declared, as it does for a
// policy field fixed at startup. It overrides a stored adapter with the same
// ID, and a stored AI adapter of the same provider; remove the declaration
// and restart, and the stored one applies again.
//
// A declaration that contradicts itself stops startup, in every category: two
// adapters handling one AI function, two AI adapters of one provider, two
// defaults in one category. A declaration that resolved one way silently is a
// configuration that does not do what its author wrote, which is worse than
// not starting. A declared adapter that fails to *configure* — a bad
// credential, an unreachable host — is logged and skipped like any other.

// AdapterDecl is one adapter declared in the config file.
type AdapterDecl struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Default  bool   `json:"is_default"`
	Enabled  bool   `json:"enabled"`

	// Config is the adapter's own settings, as it would be stored in
	// adapter_configs.config. Never a credential.
	Config map[string]any `json:"-"`

	// Credentials name where each credential is read from at startup. The
	// value itself is never in the file (R-190).
	Credentials map[string]CredentialRef `json:"-"`

	// Functions are the AI functions this adapter handles, for an AI adapter.
	Functions []FunctionDecl `json:"functions,omitempty"`

	Source Source `json:"source"`
}

// CredentialRef is where a declared credential is read from: an environment
// variable, or a file such as a mounted Docker or Kubernetes secret.
type CredentialRef struct {
	Env  string
	File string
}

// FunctionDecl is one AI function a declared adapter handles.
type FunctionDecl struct {
	Function string `json:"function"`
	Model    string `json:"model,omitempty"`
	Source   Source `json:"source"`
}

var adapterID = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// adaptersOf reads and checks the `adapters:` section.
func adaptersOf(v *viper.Viper, path string) ([]AdapterDecl, error) {
	if path == "" || !v.InConfig("adapters") {
		return nil, nil
	}
	section, ok := v.Get("adapters").(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the config file %s has an adapters section that is not a map of adapter IDs to adapters. "+
			"Valid answer: adapters: {ai_anthropic: {category: ai, kind: anthropic}}", path)
	}

	ids := make([]string, 0, len(section))
	for id := range section {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]AdapterDecl, 0, len(ids))
	for _, id := range ids {
		d, err := adapterOf(path, id, section[id])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := overlaps(path, out); err != nil {
		return nil, err
	}
	return out, nil
}

func adapterOf(path, id string, raw any) (AdapterDecl, error) {
	key := "adapters." + id
	at := func(format string, args ...any) error {
		return fmt.Errorf("the config file %s, at %s: %s", path, key, fmt.Sprintf(format, args...))
	}

	if !adapterID.MatchString(id) {
		return AdapterDecl{}, at("%q is not an adapter ID. Use lowercase letters, digits and underscores, starting with a letter, such as ai_anthropic", id)
	}
	body, ok := raw.(map[string]any)
	if !ok {
		return AdapterDecl{}, at("an adapter is a map with at least a category and a kind")
	}

	d := AdapterDecl{ID: id, Enabled: true, Source: Source{Kind: "file", Name: path, Key: key}}
	for field, value := range body {
		switch field {
		case "category":
			d.Category, _ = value.(string)
		case "kind":
			d.Kind, _ = value.(string)
		case "name":
			d.Name, _ = value.(string)
		case "default":
			b, ok := value.(bool)
			if !ok {
				return AdapterDecl{}, at("default must be true or false")
			}
			d.Default = b
		case "enabled":
			b, ok := value.(bool)
			if !ok {
				return AdapterDecl{}, at("enabled must be true or false")
			}
			d.Enabled = b
		case "config":
			m, ok := value.(map[string]any)
			if !ok {
				return AdapterDecl{}, at("config must be a map of the adapter's settings")
			}
			d.Config = m
		case "credentials":
			creds, err := credentialsOf(value)
			if err != nil {
				return AdapterDecl{}, at("%s", err.Error())
			}
			d.Credentials = creds
		case "functions":
			fns, err := functionsOf(path, key, value)
			if err != nil {
				return AdapterDecl{}, err
			}
			d.Functions = fns
		default:
			return AdapterDecl{}, at("%q is not an adapter setting. The settings are: category, kind, name, default, enabled, config, credentials, functions", field)
		}
	}

	if d.Category == "" || d.Kind == "" {
		return AdapterDecl{}, at("an adapter needs a category and a kind, such as category: ai and kind: anthropic")
	}
	if !knownCategory(d.Category) {
		return AdapterDecl{}, at("%q is not an adapter category. The categories are: %s", d.Category, strings.Join(categories(), ", "))
	}
	if d.Name == "" {
		d.Name = d.ID
	}
	if field := inlineCredential(d.Config); field != "" {
		return AdapterDecl{}, at("config contains %q, which looks like a credential. The config file is not encrypted, so Pando does not read credentials from it. "+
			"Name where to read it instead: credentials: {%s: {env: VARIABLE}} or credentials: {%s: {file: /run/secrets/name}}", field, field, field)
	}
	if len(d.Credentials) > 0 && d.Category == string(api.CategorySecrets) {
		return AdapterDecl{}, at("a secrets adapter cannot be given credentials")
	}
	if len(d.Functions) > 0 && d.Category != string(api.CategoryAI) {
		return AdapterDecl{}, at("functions are AI functions, and this is a %s adapter", d.Category)
	}
	return d, nil
}

// credentialsOf reads credentials: each is {env: NAME} or {file: PATH}. A bare
// string is a credential written into the file, which R-190 forbids.
func credentialsOf(value any) (map[string]CredentialRef, error) {
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("credentials must be a map of credential names to {env: VARIABLE} or {file: PATH}")
	}
	out := make(map[string]CredentialRef, len(m))
	for field, raw := range m {
		ref, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("credentials.%s is written inline. The config file is not encrypted, so Pando does not read credentials from it. "+
				"Name where to read it instead: {env: VARIABLE} or {file: /run/secrets/name}", field)
		}
		var c CredentialRef
		for k, v := range ref {
			s, _ := v.(string)
			switch k {
			case "env":
				c.Env = s
			case "file":
				c.File = s
			default:
				return nil, fmt.Errorf("credentials.%s has %q; a credential is read from env or file", field, k)
			}
		}
		if (c.Env == "") == (c.File == "") {
			return nil, fmt.Errorf("credentials.%s must name exactly one of env or file", field)
		}
		out[field] = c
	}
	return out, nil
}

// functionsOf reads functions: a list of names, or a map of names to
// {model: …} (or nothing, for the adapter's own model).
func functionsOf(path, key string, value any) ([]FunctionDecl, error) {
	at := func(format string, args ...any) error {
		return fmt.Errorf("the config file %s, at %s.functions: %s", path, key, fmt.Sprintf(format, args...))
	}
	var out []FunctionDecl
	add := func(name, model string) error {
		if !api.IsAssignable(api.AIFunction(name)) {
			names := make([]string, 0, len(api.AIFunctions()))
			for _, f := range api.AIFunctions() {
				names = append(names, string(f))
			}
			return at("%q is not an AI function. The functions are: %s", name, strings.Join(names, ", "))
		}
		out = append(out, FunctionDecl{Function: name, Model: model,
			Source: Source{Kind: "file", Name: path, Key: key + ".functions." + name}})
		return nil
	}

	switch fns := value.(type) {
	case []any:
		for _, item := range fns {
			name, ok := item.(string)
			if !ok {
				return nil, at("each function in a list is a name, such as repair_plan")
			}
			if err := add(name, ""); err != nil {
				return nil, err
			}
		}
	case map[string]any:
		names := make([]string, 0, len(fns))
		for name := range fns {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			model := ""
			switch body := fns[name].(type) {
			case nil:
			case map[string]any:
				for k, v := range body {
					if k != "model" {
						return nil, at("%s has %q; a function's only setting is model", name, k)
					}
					model, _ = v.(string)
				}
			default:
				return nil, at("%s must be {} or {model: NAME}", name)
			}
			if err := add(name, model); err != nil {
				return nil, err
			}
		}
	default:
		return nil, at("functions is a list of AI function names, or a map of names to {model: NAME}")
	}
	return out, nil
}

// overlaps refuses a declaration that contradicts itself (R-271). Each error
// names both declarations, so the fix is to delete one of two lines.
func overlaps(path string, decls []AdapterDecl) error {
	defaults := map[string]string{}
	aiKinds := map[string]string{}
	functions := map[string]string{}
	for _, d := range decls {
		if !d.Enabled {
			continue
		}
		if d.Default {
			if other, dup := defaults[d.Category]; dup {
				return fmt.Errorf("the config file %s declares two default %s adapters, at %s and %s. "+
					"A category has one default: remove default: true from one of them", path, d.Category, other, d.Source.Key)
			}
			defaults[d.Category] = d.Source.Key
		}
		if d.Category == string(api.CategoryAI) {
			if other, dup := aiKinds[d.Kind]; dup {
				return fmt.Errorf("the config file %s declares two %s AI adapters, at %s and %s. "+
					"Pando allows one AI adapter per provider: remove one, and give functions that need a different model a model of their own", path, d.Kind, other, d.Source.Key)
			}
			aiKinds[d.Kind] = d.Source.Key
		}
		for _, f := range d.Functions {
			if other, dup := functions[f.Function]; dup {
				return fmt.Errorf("the config file %s assigns the AI function %s twice, at %s and %s. "+
					"Each AI function is handled by one adapter: remove it from one of them", path, f.Function, other, f.Source.Key)
			}
			functions[f.Function] = f.Source.Key
		}
	}
	return nil
}

// inlineCredential names a key in an adapter's settings that looks like a
// credential — the same names POST /adapters refuses.
func inlineCredential(cfg map[string]any) string {
	keys := make([]string, 0, len(cfg))
	for k := range cfg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch strings.ToLower(k) {
		case "credentials", "api_key", "apikey", "token", "secret", "password", "access_key", "secret_key":
			return k
		}
	}
	return ""
}

func categories() []string {
	return []string{"runtime", "routing", "builder", "secrets", "services", "identity", "notify", "backup", "scanner", "ai", "image_registry", "audit_sink"}
}

func knownCategory(c string) bool {
	for _, have := range categories() {
		if have == c {
			return true
		}
	}
	return false
}

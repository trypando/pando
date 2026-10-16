package httpapi

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// envReferences refuses an adapter configured through the API that would read
// a variable from Pando's environment and send it somewhere its configurer
// chose (R-080a, the 2026-10-16 assessment's finding 5).
//
// An AI adapter's api_key_env names a variable whose value goes to base_url
// as the key. Set through the API, that let install.adapters.manage read any
// of Pando's own variables — PANDO_DATABASE_URL, PANDO_ADMIN_PASSWORD — by
// naming it and pointing base_url at a server they run. So, through the API:
//
//   - a setting ending in _env may name only the variable the kind itself
//     defaults to (ANTHROPIC_API_KEY for Anthropic), and none for a kind that
//     declares no default; and
//   - an adapter that reads its key from the environment when none is stored
//     may not be pointed at another base_url unless it has a stored key, or
//     the operator's provider key would go to that address.
//
// The configuration file is the operator's own, and may name any variable
// (R-271): whoever edits it already holds the environment.
func (s *Server) envReferences(ctx context.Context, id, category, kind string, raw json.RawMessage,
	creds map[string]secret.Value,
) error {
	cfg := configObject(raw)
	if cfg == nil {
		return nil // empty, or not an object, which inlineCredential has already refused
	}
	info, _ := s.kindInfo(category, kind)
	defaults := map[string]string{}
	for _, f := range info.Fields {
		defaults[f.Key] = f.Default
	}
	if err := envVariables(cfg, defaults); err != nil {
		return err
	}
	return s.envKeyDestination(ctx, id, cfg, defaults, creds)
}

// envVariables refuses a *_env setting naming any variable but the one the
// kind defaults to.
func envVariables(cfg map[string]json.RawMessage, defaults map[string]string) error {
	for key, value := range cfg {
		if !strings.HasSuffix(strings.ToLower(key), "_env") {
			continue
		}
		var name string
		if json.Unmarshal(value, &name) != nil || name == "" {
			continue
		}
		want := defaults[key]
		if want != "" && name == want {
			continue
		}
		allowed := "no variable"
		if want != "" {
			allowed = "only " + want
		}
		return errs.Newf(errs.ValidInvalid,
			"The adapter's %s names %s, an environment variable Pando would read and send to the provider as a key. "+
				"Set through the API, %s may name %s, so that no other variable in Pando's environment can be sent anywhere.",
			key, name, key, allowed).
			WithRemedy(`Store the key as this adapter's credential instead, in the request's "credentials" object, ` +
				"or declare the adapter in Pando's configuration file, where any variable may be named.")
	}
	return nil
}

// envKeyDestination refuses sending a key read from the environment to an
// address other than the provider's. An adapter reads its key from the
// environment when none is stored for it or sent in this request, if its kind
// declares a variable to read.
func (s *Server) envKeyDestination(ctx context.Context, id string, cfg map[string]json.RawMessage,
	defaults map[string]string, creds map[string]secret.Value,
) error {
	if defaults["api_key_env"] == "" {
		return nil
	}
	var baseURL string
	if v, ok := cfg["base_url"]; ok {
		_ = json.Unmarshal(v, &baseURL)
	}
	if baseURL == "" || sameURL(baseURL, defaults["base_url"]) {
		return nil
	}
	if stored, err := s.hasStoredKey(ctx, id, creds); err != nil || stored {
		return err
	}
	return errs.Newf(errs.ValidInvalid,
		"This adapter has no stored API key, so it would read %s from Pando's environment and send it to %s. "+
			"Set through the API, an adapter pointed at an address other than its provider's needs a key of its own.",
		defaults["api_key_env"], baseURL).
		WithRemedy(`Send the key for that address in the request's "credentials" object, for example ` +
			`{"credentials": {"api_key": "…"}}, or declare the adapter in Pando's configuration file.`)
}

// hasStoredKey reports whether the adapter will have an api_key credential
// once this request's credentials are saved.
func (s *Server) hasStoredKey(ctx context.Context, id string, creds map[string]secret.Value) (bool, error) {
	if v, ok := creds["api_key"]; ok {
		return !v.IsZero(), nil
	}
	if s.AdapterCredentials == nil {
		return false, nil
	}
	fields, err := s.AdapterCredentials.Fields(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(fields[id], "api_key"), nil
}

func (s *Server) kindInfo(category, kind string) (api.KindInfo, bool) {
	for _, k := range s.AdapterKinds {
		if string(k.Category) == category && k.Kind == kind {
			return k, true
		}
	}
	return api.KindInfo{}, false
}

// configObject is an adapter's configuration as an object, or nil.
func configObject(raw json.RawMessage) map[string]json.RawMessage {
	var cfg map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &cfg) != nil {
		return nil
	}
	return cfg
}

func sameURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(a), "/"), strings.TrimRight(strings.TrimSpace(b), "/"))
}

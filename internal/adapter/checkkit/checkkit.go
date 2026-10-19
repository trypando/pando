// Package checkkit lets a kind of adapter check a configuration as it is
// saved, by running its own Configure on it, without anything stored.
//
// Configure reads credentials from a `credentials` object beside the
// settings, as Pando hands them over at startup. A credential already stored
// and not sent again is given a stand-in value, so a check that it is there
// passes; a check of what a stored credential says is the kind's to skip. One
// sent empty is being removed, and is left out.
package checkkit

import (
	"encoding/json"

	"github.com/trypando/pando/internal/secret"
)

// Stored stands in for a credential that is kept and not sent again.
const Stored = "stored"

// Merge is the configuration Configure reads: the settings, with the
// credentials sent, and a stand-in for each one stored and not sent.
func Merge(config json.RawMessage, credentials map[string]secret.Value, stored []string) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &cfg); err != nil {
			return nil, err
		}
	}
	if cfg == nil { // "config": null
		cfg = map[string]json.RawMessage{}
	}
	plain := map[string]string{}
	for _, field := range stored {
		plain[field] = Stored
	}
	for field, v := range credentials {
		if v.IsZero() {
			delete(plain, field) // sent empty: removed
		} else {
			plain[field] = v.Reveal()
		}
	}
	if len(plain) > 0 {
		body, err := json.Marshal(plain)
		if err != nil {
			return nil, err
		}
		cfg["credentials"] = body
	}
	return json.Marshal(cfg)
}

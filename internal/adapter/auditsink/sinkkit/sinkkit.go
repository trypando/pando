// Package sinkkit holds what the audit sink kinds share: the settings that
// narrow and shape what core sends (R-384), reading inline credentials out of
// plain configuration (R-190), and a trusted certificate bundle.
//
// It is not a kind. Each kind under internal/adapter/auditsink uses it the
// way source kinds use forgekit.
package sinkkit

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
)

// Prefixes is a list of action prefixes, given as one comma-separated string
// (what a form sends) or as a JSON array (what a script might).
type Prefixes []string

// UnmarshalJSON accepts "app.use, deploy." or ["app.use", "deploy."].
func (p *Prefixes) UnmarshalJSON(b []byte) error {
	var list []string
	if err := json.Unmarshal(b, &list); err != nil {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("action prefixes are a comma-separated list, such as app.use, deploy")
		}
		list = strings.Split(s, ",")
	}
	var out Prefixes
	for _, v := range list {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	*p = out
	return nil
}

// Common is the settings every audit sink takes, read beside the kind's own.
type Common struct {
	Format   string   `json:"format,omitempty"`
	MaxBatch int      `json:"max_batch,omitempty"`
	Actions  Prefixes `json:"actions,omitempty"`
	Exclude  Prefixes `json:"exclude,omitempty"`
	Start    string   `json:"start,omitempty"`
}

// Normalize fills defaults and refuses values core cannot act on. kind names
// the adapter in each message.
func (c *Common) Normalize(kind string, defaultBatch int) error {
	switch c.Format {
	case "":
		c.Format = api.AuditFormatNative
	case api.AuditFormatNative, api.AuditFormatOCSF:
	default:
		return fmt.Errorf("%s: %q is not an event format. Valid answers: native (the event as Pando stores it) or ocsf (OCSF 1.3.0)", kind, c.Format)
	}
	switch c.Start {
	case "":
		c.Start = "oldest"
	case "oldest", "now":
	default:
		return fmt.Errorf("%s: %q is not a starting point. Valid answers: oldest (the oldest event in the live log) or now (only events from now on)", kind, c.Start)
	}
	if c.MaxBatch < 0 {
		return fmt.Errorf("%s: max_batch is %d. Set it to a positive number of events per delivery, such as %d, or leave it empty", kind, c.MaxBatch, defaultBatch)
	}
	if c.MaxBatch == 0 {
		c.MaxBatch = defaultBatch
	}
	return nil
}

// Capabilities is the part of AuditSinkCapabilities these settings answer.
func (c Common) Capabilities(transport, endpoint string) api.AuditSinkCapabilities {
	return api.AuditSinkCapabilities{
		MaxBatch:   c.MaxBatch,
		Format:     c.Format,
		Transport:  transport,
		Endpoint:   endpoint,
		Actions:    c.Actions,
		Exclude:    c.Exclude,
		StartAtNow: c.Start == "now",
	}
}

// RefuseInline refuses a configuration carrying any of keys at its top level,
// which is the unencrypted part of an adapter's row (R-190). Credentials
// arrive under "credentials", sealed.
func RefuseInline(kind string, raw json.RawMessage, keys ...string) error {
	if len(raw) == 0 {
		return nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return fmt.Errorf("%s: reading configuration: %w", kind, err)
	}
	for _, k := range keys {
		if _, inline := top[k]; inline {
			return fmt.Errorf("%s: %s is in this adapter's stored configuration, which is unencrypted; set it as a credential instead", kind, k)
		}
	}
	return nil
}

// CertPool reads a PEM bundle setting. Empty means the system's roots: a nil
// pool, which crypto/tls reads that way.
func CertPool(kind, pem string) (*x509.CertPool, error) {
	pem = strings.TrimSpace(pem)
	if pem == "" {
		return nil, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem)) {
		return nil, fmt.Errorf("%s: the CA certificate has no certificate Pando can read. Paste one or more PEM certificates, each beginning -----BEGIN CERTIFICATE-----", kind)
	}
	return pool, nil
}

// Fields are the settings Common reads, for a kind's Info.
func Fields(defaultBatch int) []api.Field {
	return []api.Field{
		{Key: "actions", Label: "Actions", Type: "string", Placeholder: "deploy, install.",
			Help: "Comma-separated action prefixes to send. Empty sends every event."},
		{Key: "exclude", Label: "Exclude", Type: "string", Placeholder: "app.use",
			Help: "Comma-separated action prefixes not to send. app.use, a record of each request to an app, is usually most of the volume; exclude it if your SIEM bills by ingest."},
		{Key: "format", Label: "Event format", Type: "select", Default: api.AuditFormatNative, Advanced: true,
			Help: "How each event is encoded.",
			Options: []api.Option{
				{Value: api.AuditFormatNative, Label: "Native", Description: "The event as Pando stores it."},
				{Value: api.AuditFormatOCSF, Label: "OCSF", Description: "OCSF 1.3.0, for a SIEM that maps it."},
			}},
		{Key: "max_batch", Label: "Batch size", Type: "int", Default: fmt.Sprint(defaultBatch), Advanced: true,
			Help: "The most events sent in one delivery."},
		{Key: "start", Label: "Start", Type: "select", Default: "oldest", Advanced: true,
			Help: "Where a new destination's first delivery begins. Read once, when Pando first sends to it.",
			Options: []api.Option{
				{Value: "oldest", Label: "The oldest event in the live log"},
				{Value: "now", Label: "Only events from now on"},
			}},
	}
}

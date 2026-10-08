package api

import (
	"context"
	"encoding/json"
)

// The audit sink category (R-252, R-382, issue #129, design 12 §5).
//
// The thirteenth category: a destination the audit log is pushed to as it is
// written — a syslog collector, a SIEM's HTTPS ingest endpoint. It passes
// design 03 §8.1's test on vocabulary: Splunk HEC's envelope and auth scheme,
// Datadog's API key header, Elastic's bulk action lines, RFC 5424 framing and
// a client certificate are a provider's, and core knows none of them.
//
// An adapter is handed events core has already read and encoded. It never
// reads the log, never sees its cursor, and cannot write audit (R-027,
// R-226): core decides what is sent and records what was delivered.

// AuditSinkAdapter delivers batches of audit events to one destination.
type AuditSinkAdapter interface {
	Adapter

	AuditSinkCapabilities() AuditSinkCapabilities

	// Send delivers a batch, in order. A nil error means the destination
	// accepted every event in it; any error means none is counted, and core
	// sends the same batch again later. Delivery is at least once, so a
	// destination may see an event twice; its id is the idempotency key.
	Send(ctx context.Context, b AuditBatch) error
}

// Audit event formats an adapter may ask for (R-384, design 12 §6).
const (
	// AuditFormatNative is the archive's line: the row as Pando stores it.
	AuditFormatNative = "native"
	// AuditFormatOCSF is OCSF 1.3.0, mapped by core's table.
	AuditFormatOCSF = "ocsf"
)

// AuditSinkCapabilities is how core feeds the adapter, as data (R-254).
type AuditSinkCapabilities struct {
	// MaxBatch is the most events core puts in one Send. Zero means 500.
	MaxBatch int `json:"max_batch"`

	// Format is what core encodes events as: AuditFormatNative or
	// AuditFormatOCSF.
	Format string `json:"format"`

	// Transport is "syslog" or "https", and Endpoint the host and port it
	// sends to — never with a credential. The console names both beside the
	// audit log (R-385): nobody reading the log should miss that a copy of it
	// is leaving.
	Transport string `json:"transport"`
	Endpoint  string `json:"endpoint"`

	// Actions and Exclude are action prefixes narrowing what core sends: an
	// event matching one of Actions (or any, when empty) and none of Exclude
	// (R-384). Configured on the adapter and reported here, so core, the
	// console and the archiver read one answer.
	Actions []string `json:"actions,omitempty"`
	Exclude []string `json:"exclude,omitempty"`

	// StartAtNow says a new destination's first delivery starts after the
	// newest event, rather than at the oldest in the live log. Read once,
	// when core first sees the destination; changing it later moves nothing.
	StartAtNow bool `json:"start_at_now,omitempty"`
}

// AuditBatch is one Send's events.
type AuditBatch struct {
	// Events are encoded in Capabilities().Format, one JSON value each,
	// oldest first.
	Events []json.RawMessage

	// IDs are the events' ids, in the same order, for a destination that
	// takes an idempotency key.
	IDs []int64

	// Actions are the events' actions, in the same order, for a transport
	// that carries one outside the body — syslog's MSGID.
	Actions []string
}

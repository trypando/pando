package id

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Kind is an object's ID prefix.
//
// The prefix is load-bearing, not decoration (design 00 §3.1): it makes log
// lines and error messages self-describing, and it makes copy-paste mistakes
// visible at a glance rather than at the point of a confusing 404.
type Kind string

const (
	App          Kind = "app"
	Spec         Kind = "spec"
	Deployment   Kind = "dep"
	User         Kind = "usr"
	Group        Kind = "grp"
	Token        Kind = "tok"
	Session      Kind = "ses"
	Role         Kind = "role"
	Grant        Kind = "gr"
	Volume       Kind = "vol"
	Secret       Kind = "sec"
	Service      Kind = "svc"
	Backup       Kind = "bkp"
	Notification Kind = "ntf"
	IdentityAdpt Kind = "idp"
	Request      Kind = "req"

	// Scan is a security scan of one spec revision (R-310).
	Scan Kind = "scan"

	// Section is a grouping in one person's launcher (R-342). Not "sec",
	// which is a secret.
	Section Kind = "sect"

	// AuditArchive is one archived month of the audit log (R-347).
	AuditArchive Kind = "aar"

	// Upgrade is one in-place upgrade of Pando itself (R-359).
	Upgrade Kind = "upg"

	// Adapter configs are prefixed by category, so a log line naming one says
	// which kind of adapter it is (design 02 §2.5).
	AdapterRuntime  Kind = "rt"
	AdapterRouting  Kind = "rte"
	AdapterBuilder  Kind = "bld"
	AdapterSecrets  Kind = "sek"
	AdapterServices Kind = "svcs"
	AdapterNotify   Kind = "ntf"
	AdapterScanner  Kind = "scn"
)

const sep = "_"

// New returns a fresh prefixed, sortable, opaque identifier: app_01HQ8...
//
// The body is a ULID, so IDs sort by creation time. That property is relied on
// for cursor pagination and makes a sorted list of IDs chronological without a
// join.
func New(k Kind) string {
	return string(k) + sep + ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
}

// Parse splits an identifier into its kind and ULID body, validating both.
func Parse(s string) (Kind, ulid.ULID, error) {
	prefix, body, found := strings.Cut(s, sep)
	if !found || prefix == "" || body == "" {
		return "", ulid.ULID{}, fmt.Errorf("malformed identifier %q: expected <prefix>_<ulid>", s)
	}
	u, err := ulid.ParseStrict(body)
	if err != nil {
		return "", ulid.ULID{}, fmt.Errorf("malformed identifier %q: %w", s, err)
	}
	return Kind(prefix), u, nil
}

// Is reports whether s is a well-formed identifier of kind k.
//
// Use it at API boundaries. Rejecting an app ID handed to a volume endpoint
// produces a clear error instead of a lookup that mysteriously finds nothing —
// which is the whole reason the prefix exists.
func Is(k Kind, s string) bool {
	got, _, err := Parse(s)
	return err == nil && got == k
}

// Time returns when an identifier was generated.
func Time(s string) (time.Time, error) {
	_, u, err := Parse(s)
	if err != nil {
		return time.Time{}, err
	}
	return ulid.Time(u.Time()).UTC(), nil
}

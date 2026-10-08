// Package ocsf encodes native audit lines as OCSF events (design 12 §6.2,
// R-384).
//
// Everything in an OCSF event is derived from the line it encodes; nothing is
// looked up. The line's detail never holds a secret (R-194), so carrying it
// whole under unmapped adds nothing a native line does not already say.
package ocsf

import (
	"encoding/json"
	"fmt"

	"github.com/trypando/pando/internal/core/audit"
)

// SchemaVersion is the OCSF schema version the events conform to.
const SchemaVersion = "1.3.0"

// ProductVersion is Pando's version, reported as metadata.product.version.
// Set by main at startup.
var ProductVersion = "dev"

// Field is one row of the base-field table: an OCSF field and the native
// column(s) it comes from.
type Field struct {
	OCSF string
	From string
}

// BaseFields is the field-by-field mapping Encode implements, in the order the
// generated reference lists it. TestR384_OCSFBaseFields holds Encode's output
// to it, so a field cannot be added to one and not the other.
var BaseFields = []Field{
	{"class_uid, class_name", "The action's row under Classes."},
	{"category_uid, category_name", "`class_uid / 1000`."},
	{"activity_id, activity_name", "The action's row under Classes."},
	{"type_uid, type_name", "`class_uid × 100 + activity_id`; the name is `<class>: <activity>`."},
	{"time", "`occurred_at`, in epoch milliseconds."},
	{"severity_id, severity", "1 Informational for `success`, 3 Medium for `denied`, 4 High for `failed`."},
	{"status_id, status", "1 Success for `success`, 2 Failure otherwise."},
	{"status_detail", "`denied` or `failed`; absent on success."},
	{"message", "`<action> by <actor> on <target_kind> <target_id>`. The actor is `actor_name`, else `principal_id`, else `principal_kind`."},
	{"metadata.uid", "`id`, as a string."},
	{"metadata.version", "`" + SchemaVersion + "`."},
	{"metadata.product", "`{name: \"Pando\", vendor_name: \"Pando\", version}`."},
	{"metadata.correlation_uid", "`request_id`, when set."},
	{"metadata.log_name", "`audit`."},
	{"actor.user", "`{uid: principal_id, type_id, type, name: actor_name, email_addr: actor_email}`. `type` is `User` (1), `System` (3) or `Token` (99 Other); absent for an anonymous principal."},
	{"actor.invoked_by", "`on_behalf_of`: the person a token acted for (R-229, R-262)."},
	{"src_endpoint.ip", "`source_ip`, when set."},
	{"http_request.user_agent", "`user_agent`, when set."},
	{"api.operation", "`action`."},
	{"resources", "`{type: target_kind, uid: target_id}`, then `{type: \"app\", uid: app_id}` when `app_id` is set and is not the target."},
	{"unmapped", "`detail` whole, `peer_ip`, `schema_version`, `principal_kind` and `on_behalf_of`."},
}

type event struct {
	ClassUID     int        `json:"class_uid"`
	ClassName    string     `json:"class_name"`
	CategoryUID  int        `json:"category_uid"`
	CategoryName string     `json:"category_name"`
	ActivityID   int        `json:"activity_id"`
	ActivityName string     `json:"activity_name"`
	TypeUID      int        `json:"type_uid"`
	TypeName     string     `json:"type_name"`
	Time         int64      `json:"time"`
	SeverityID   int        `json:"severity_id"`
	Severity     string     `json:"severity"`
	StatusID     int        `json:"status_id"`
	Status       string     `json:"status"`
	StatusDetail string     `json:"status_detail,omitempty"`
	Message      string     `json:"message"`
	Metadata     metadata   `json:"metadata"`
	Actor        *actor     `json:"actor,omitempty"`
	SrcEndpoint  *endpoint  `json:"src_endpoint,omitempty"`
	HTTPRequest  *request   `json:"http_request,omitempty"`
	API          api        `json:"api"`
	Resources    []resource `json:"resources"`
	Unmapped     unmapped   `json:"unmapped"`
}

type metadata struct {
	UID            string  `json:"uid"`
	Version        string  `json:"version"`
	Product        product `json:"product"`
	CorrelationUID string  `json:"correlation_uid,omitempty"`
	LogName        string  `json:"log_name"`
}

type product struct {
	Name       string `json:"name"`
	VendorName string `json:"vendor_name"`
	Version    string `json:"version"`
}

type actor struct {
	User      *user  `json:"user,omitempty"`
	InvokedBy string `json:"invoked_by,omitempty"`
}

type user struct {
	UID       string `json:"uid,omitempty"`
	TypeID    int    `json:"type_id"`
	Type      string `json:"type"`
	Name      string `json:"name,omitempty"`
	EmailAddr string `json:"email_addr,omitempty"`
}

type endpoint struct {
	IP string `json:"ip"`
}

type request struct {
	UserAgent string `json:"user_agent"`
}

type api struct {
	Operation string `json:"operation"`
}

type resource struct {
	Type string `json:"type"`
	UID  string `json:"uid,omitempty"`
}

type unmapped struct {
	Detail        json.RawMessage `json:"detail,omitempty"`
	PeerIP        string          `json:"peer_ip,omitempty"`
	SchemaVersion int             `json:"schema_version"`
	PrincipalKind string          `json:"principal_kind"`
	OnBehalfOf    string          `json:"on_behalf_of,omitempty"`
}

var _ audit.Encoder = Encode

// Encode is an audit.Encoder: one native line in, one OCSF event out.
func Encode(line json.RawMessage) (json.RawMessage, error) {
	var l audit.Line
	if err := json.Unmarshal(line, &l); err != nil {
		return nil, fmt.Errorf("ocsf: decode audit line: %w", err)
	}
	// detail again, as the bytes it was written as: decoding into a map
	// would turn every number into a float and lose precision.
	var raw struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(line, &raw); err != nil {
		return nil, fmt.Errorf("ocsf: decode audit line detail: %w", err)
	}
	if string(raw.Detail) == "null" {
		raw.Detail = nil
	}

	m, _ := Lookup(l.Action)
	outcome := outcomeOf(l)
	targetKind, targetID := target(l)

	e := event{
		ClassUID:     m.ClassUID,
		ClassName:    m.ClassName,
		CategoryUID:  m.CategoryUID(),
		CategoryName: m.CategoryName(),
		ActivityID:   m.ActivityID,
		ActivityName: m.ActivityName,
		TypeUID:      m.TypeUID(),
		TypeName:     m.TypeName(),
		Time:         l.OccurredAt.UnixMilli(),
		Message:      message(l, targetKind, targetID),
		Metadata: metadata{
			UID:            fmt.Sprint(l.ID),
			Version:        SchemaVersion,
			Product:        product{Name: "Pando", VendorName: "Pando", Version: ProductVersion},
			CorrelationUID: audit.Str(l.RequestID),
			LogName:        "audit",
		},
		Actor:     actorOf(l),
		API:       api{Operation: l.Action},
		Resources: resources(l, targetKind, targetID),
		Unmapped: unmapped{
			Detail:        raw.Detail,
			PeerIP:        audit.Str(l.PeerIP),
			SchemaVersion: l.Version(),
			PrincipalKind: l.PrincipalKind,
			OnBehalfOf:    audit.Str(l.OnBehalfOf),
		},
	}
	switch outcome {
	case audit.OutcomeDenied:
		e.SeverityID, e.Severity = 3, "Medium"
		e.StatusID, e.Status, e.StatusDetail = 2, "Failure", string(audit.OutcomeDenied)
	case audit.OutcomeFailed:
		e.SeverityID, e.Severity = 4, "High"
		e.StatusID, e.Status, e.StatusDetail = 2, "Failure", string(audit.OutcomeFailed)
	default:
		e.SeverityID, e.Severity = 1, "Informational"
		e.StatusID, e.Status = 1, "Success"
	}
	if ip := audit.Str(l.SourceIP); ip != "" {
		e.SrcEndpoint = &endpoint{IP: ip}
	}
	if ua := audit.Str(l.UserAgent); ua != "" {
		e.HTTPRequest = &request{UserAgent: ua}
	}

	out, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("ocsf: encode event %d: %w", l.ID, err)
	}
	return out, nil
}

// outcomeOf is the recorded outcome, or for a row from before R-379 (or one
// holding a value this version does not know) the one its action name says.
func outcomeOf(l audit.Line) audit.Outcome {
	switch o := audit.Outcome(audit.Str(l.Outcome)); o {
	case audit.OutcomeSuccess, audit.OutcomeDenied, audit.OutcomeFailed:
		return o
	}
	return audit.OutcomeOf(l.Action)
}

// target is what the event is about. Rows from before R-379 can have no
// target_kind; they are read the way the writer now fills it in: the app,
// else the installation.
func target(l audit.Line) (kind, id string) {
	kind, id = audit.Str(l.TargetKind), audit.Str(l.TargetID)
	switch {
	case kind != "":
		return kind, id
	case audit.Str(l.AppID) != "":
		return "app", audit.Str(l.AppID)
	default:
		return "install", "install"
	}
}

func message(l audit.Line, targetKind, targetID string) string {
	who := audit.Str(l.ActorName)
	if who == "" {
		who = audit.Str(l.PrincipalID)
	}
	if who == "" {
		who = l.PrincipalKind
	}
	if who == "" {
		who = "anonymous"
	}
	on := targetKind
	if targetID != "" {
		on += " " + targetID
	}
	return l.Action + " by " + who + " on " + on
}

func actorOf(l audit.Line) *actor {
	a := actor{InvokedBy: audit.Str(l.OnBehalfOf)}
	u := user{
		UID:       audit.Str(l.PrincipalID),
		Name:      audit.Str(l.ActorName),
		EmailAddr: audit.Str(l.ActorEmail),
	}
	switch l.PrincipalKind {
	case "user":
		u.TypeID, u.Type = 1, "User"
		a.User = &u
	case "system":
		u.TypeID, u.Type = 3, "System"
		a.User = &u
	case "token":
		u.TypeID, u.Type = 99, "Token"
		a.User = &u
	}
	if a.User == nil && a.InvokedBy == "" {
		return nil
	}
	return &a
}

func resources(l audit.Line, targetKind, targetID string) []resource {
	rs := []resource{{Type: targetKind, UID: targetID}}
	if app := audit.Str(l.AppID); app != "" && app != targetID {
		rs = append(rs, resource{Type: "app", UID: app})
	}
	return rs
}

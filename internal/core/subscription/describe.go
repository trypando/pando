package subscription

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/core/state"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// LinkFor is where in the console an event is seen: the app's page for an app
// event, the Events screen otherwise. Empty when Pando does not know its own
// address (external_url).
func LinkFor(externalURL, appID string) string {
	base := strings.TrimRight(strings.TrimSpace(externalURL), "/")
	if base == "" {
		return ""
	}
	if appID != "" {
		return base + "/admin/apps/" + appID + "/events"
	}
	return base + "/admin/events"
}

// Describe is an event as a message a person reads: a subject naming what
// happened and to what, and the event's fields under it. Notify adapters lay
// these out for their platform; none of them has to know the catalog.
func Describe(e state.Event, app *EnvelopeApp) api.Notification {
	def, _ := events.Lookup(e.Name)
	about := "Pando"
	if app != nil && app.Name != "" {
		about = app.Name
	}

	subject := about + ": " + strings.TrimSuffix(def.Summary, ".")
	switch e.Name {
	case events.AppStateChanged:
		subject = fmt.Sprintf("%s is %s (was %s)", about, str(e.Data["to"]), str(e.Data["from"]))
	case events.DeployFailed:
		subject = about + ": a deploy failed"
	case events.DeploySucceeded:
		subject = about + ": deployed"
	case "app.failed":
		subject = about + " has failed and Pando has stopped restarting it"
	case "security.scanned":
		subject = fmt.Sprintf("%s scored %s", about, str(e.Data["score"]))
	case events.SubscriptionTest:
		subject = "Pando test delivery"
	}
	if def.Name == "" {
		subject = about + ": " + e.Name
	}

	fields := fieldsOf(def, e.Data)
	var body strings.Builder
	if msg := str(e.Data["message"]); msg != "" {
		body.WriteString(msg)
	} else if r := str(e.Data["reason"]); r != "" {
		body.WriteString(r)
	} else {
		body.WriteString(def.Summary)
	}
	if remedy := str(e.Data["remedy"]); remedy != "" {
		body.WriteString("\n\n" + remedy)
	}

	n := api.Notification{
		Kind:    api.NotificationKind(e.Name),
		AppID:   e.AppID,
		Subject: subject,
		Body:    body.String(),
		EventID: e.ID,
		Fields:  fields,
	}
	return n
}

// fieldsOf lists an event's data in catalog order, then anything else sorted,
// skipping the fields the body already says.
func fieldsOf(def events.Def, data map[string]any) []api.NotificationField {
	skip := map[string]bool{"message": true, "reason": true, "remedy": true}
	var out []api.NotificationField
	seen := map[string]bool{}
	for _, f := range def.Fields {
		seen[f.Name] = true
		if v, ok := data[f.Name]; ok && !skip[f.Name] && str(v) != "" {
			out = append(out, api.NotificationField{Label: label(f.Name), Value: str(v)})
		}
	}
	var rest []string
	for k := range data {
		if !seen[k] && !skip[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		if v := str(data[k]); v != "" {
			out = append(out, api.NotificationField{Label: label(k), Value: v})
		}
	}
	return out
}

func label(name string) string {
	s := strings.ReplaceAll(name, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func str(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	case []any:
		parts := make([]string, 0, len(t))
		for _, p := range t {
			parts = append(parts, str(p))
		}
		return strings.Join(parts, ", ")
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprint(t)
		}
		return string(b)
	}
}
